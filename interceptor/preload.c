// libgcr-interceptor.so — selective CUDA-interception shim (LD_PRELOAD).
//
// Realizes the GCR *idea* with our own code (no upstream GCR hook).
//
// LEVEL 1, STEP 1 (this file): add CUDA Virtual Memory Management (VMM) hooks and
// a segment registry. PyTorch with PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True
// allocates GPU memory via the VMM driver API (cuMemCreate / cuMemMap / ...), which
// is exactly the API GCR needs to copy data to host AND free physical memory while
// PRESERVING the virtual address. In this step we ONLY OBSERVE: track the segments
// and, on a checkpoint signal, log what we captured. No device memory is copied,
// unmapped, or freed yet (that is step 2/3/4).
//
// We also keep the legacy cudaMalloc/cuMemAlloc_v2 hooks so non-VMM allocations
// remain visible. No CUDA headers needed — we match the stable CUDA ABI directly.

#define _GNU_SOURCE
#include <dlfcn.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>
#include <sys/mman.h>
#include <fcntl.h>

#define GCR_IDLE 0
#define GCR_CKPT 1
#define GCR_RESTORE 2
#define GCR_GATING 3   // interceptor -> orchestrator: gate is up (data remap in progress)

// ---- real dlsym (to call through our own dlsym override without recursion) ----
static void *(*real_dlsym)(void *, const char *) = NULL;
static void ensure_real_dlsym(void) {
    if (real_dlsym) return;
    real_dlsym = (void *(*)(void *, const char *))dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.2.5");
    if (!real_dlsym) real_dlsym = (void *(*)(void *, const char *))dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34");
}
static void *gcr_next(const char *sym) { ensure_real_dlsym(); return real_dlsym ? real_dlsym(RTLD_NEXT, sym) : NULL; }

// ---- CUDA VMM ABI (declared locally; no CUDA headers) -------------------
typedef int                CUresult_t;     // CUresult enum -> int
typedef unsigned long long CUdeviceptr_t;  // 64-bit device pointer
typedef unsigned long long CUmemHandle_t;  // CUmemGenericAllocationHandle

// CUmemAllocationProp ABI (32 bytes on x86-64). Stored so step 4 (restore) can
// recreate physical memory with identical properties.
typedef struct {
    int   type;                  // CUmemAllocationType
    int   requestedHandleTypes;  // CUmemAllocationHandleType
    struct { int type; int id; } location;  // CUmemLocation
    void *win32HandleMetaData;
    struct {
        unsigned char  compressionType;
        unsigned char  gpuDirectRDMACapable;
        unsigned short usage;
        unsigned char  reserved[4];
    } allocFlags;
} gcr_mem_prop_t;

// ---- legacy (cudaMalloc / cuMemAlloc_v2) registry -----------------------
typedef struct { void *ptr; size_t size; int live; } gcr_alloc_t;
#define GCR_MAX_ALLOCS 65536
static gcr_alloc_t g_allocs[GCR_MAX_ALLOCS];
static atomic_size_t g_count = 0;
static pthread_mutex_t g_lock = PTHREAD_MUTEX_INITIALIZER;

static void reg_add(void *ptr, size_t size) {
    if (!ptr) return;
    pthread_mutex_lock(&g_lock);
    size_t n = atomic_load(&g_count);
    if (n < GCR_MAX_ALLOCS) { g_allocs[n].ptr = ptr; g_allocs[n].size = size; g_allocs[n].live = 1; atomic_store(&g_count, n + 1); }
    pthread_mutex_unlock(&g_lock);
}
static void reg_del(void *ptr) {
    if (!ptr) return;
    pthread_mutex_lock(&g_lock);
    size_t n = atomic_load(&g_count);
    for (size_t i = 0; i < n; i++) if (g_allocs[i].live && g_allocs[i].ptr == ptr) { g_allocs[i].live = 0; break; }
    pthread_mutex_unlock(&g_lock);
}

// ---- VMM registry: physical handles + mapped segments -------------------
typedef struct { CUmemHandle_t handle; size_t size; gcr_mem_prop_t prop; int live; } gcr_phys_t;
typedef struct { CUdeviceptr_t va; size_t size; size_t offset; CUmemHandle_t handle; int live; } gcr_seg_t;
#define GCR_MAX_VMM 131072
static gcr_phys_t g_phys[GCR_MAX_VMM]; static atomic_size_t g_phys_n = 0;
static gcr_seg_t  g_seg[GCR_MAX_VMM];  static atomic_size_t g_seg_n  = 0;
static pthread_mutex_t g_vmm_lock = PTHREAD_MUTEX_INITIALIZER;

static void vmm_phys_add(CUmemHandle_t h, size_t size, const void *prop) {
    pthread_mutex_lock(&g_vmm_lock);
    size_t n = atomic_load(&g_phys_n);
    if (n < GCR_MAX_VMM) {
        g_phys[n].handle = h; g_phys[n].size = size; g_phys[n].live = 1;
        if (prop) memcpy(&g_phys[n].prop, prop, sizeof(gcr_mem_prop_t));
        atomic_store(&g_phys_n, n + 1);
    }
    pthread_mutex_unlock(&g_vmm_lock);
    fprintf(stderr, "[gcr][vmm] cuMemCreate handle=%llu size=%zu\n", (unsigned long long)h, size);
    fflush(stderr);
}
static void vmm_seg_add(CUdeviceptr_t va, size_t size, size_t off, CUmemHandle_t h) {
    pthread_mutex_lock(&g_vmm_lock);
    size_t n = atomic_load(&g_seg_n);
    if (n < GCR_MAX_VMM) { g_seg[n].va = va; g_seg[n].size = size; g_seg[n].offset = off; g_seg[n].handle = h; g_seg[n].live = 1; atomic_store(&g_seg_n, n + 1); }
    pthread_mutex_unlock(&g_vmm_lock);
    fprintf(stderr, "[gcr][vmm] cuMemMap va=0x%llx size=%zu handle=%llu\n", (unsigned long long)va, size, (unsigned long long)h);
    fflush(stderr);
}
static void vmm_seg_del(CUdeviceptr_t va) {
    pthread_mutex_lock(&g_vmm_lock);
    size_t n = atomic_load(&g_seg_n);
    for (size_t i = 0; i < n; i++) if (g_seg[i].live && g_seg[i].va == va) { g_seg[i].live = 0; break; }
    pthread_mutex_unlock(&g_vmm_lock);
}
static void vmm_phys_del(CUmemHandle_t h) {
    pthread_mutex_lock(&g_vmm_lock);
    size_t n = atomic_load(&g_phys_n);
    for (size_t i = 0; i < n; i++) if (g_phys[i].live && g_phys[i].handle == h) { g_phys[i].live = 0; break; }
    pthread_mutex_unlock(&g_vmm_lock);
}

// STEP 1: observe only — log the live VMM segments captured at checkpoint time.
static void vmm_dump(void) {
    pthread_mutex_lock(&g_vmm_lock);
    size_t sn = atomic_load(&g_seg_n), live = 0, bytes = 0;
    for (size_t i = 0; i < sn; i++) if (g_seg[i].live) { live++; bytes += g_seg[i].size; }
    fprintf(stderr, "[gcr][vmm] checkpoint OBSERVE: %zu live segments, %zu bytes "
                    "(phys handles seen=%zu, seg events=%zu)\n",
            live, bytes, (size_t)atomic_load(&g_phys_n), sn);
    size_t shown = 0;
    for (size_t i = 0; i < sn && shown < 16; i++) if (g_seg[i].live) {
        fprintf(stderr, "[gcr][vmm]   seg va=0x%llx size=%zu handle=%llu\n",
                (unsigned long long)g_seg[i].va, g_seg[i].size, (unsigned long long)g_seg[i].handle);
        shown++;
    }
    pthread_mutex_unlock(&g_vmm_lock);
    fflush(stderr);
}

// ---- control channel ----------------------------------------------------
static char g_ctrl_path[1024], g_info_path[1024], g_data_dir[1024];

static void build_paths(void) {
    const char *cdir = getenv("GCR_CONTROL_DIR"); if (!cdir || !*cdir) cdir = "/var/lib/gpu-cr/run";
    const char *ddir = getenv("GCR_DATA_DIR");    if (!ddir || !*ddir) ddir = cdir;
    const char *uid  = getenv("GCR_POD_UID");     if (!uid  || !*uid ) uid  = "default";
    snprintf(g_ctrl_path, sizeof(g_ctrl_path), "%s/%s/control", cdir, uid);
    snprintf(g_info_path, sizeof(g_info_path), "%s/%s/intercepted-info", cdir, uid);
    snprintf(g_data_dir,  sizeof(g_data_dir),  "%s/%s", ddir, uid);
}

static int read_signal(void) {
    FILE *f = fopen(g_ctrl_path, "r"); if (!f) return -1;
    int v = -1; if (fscanf(f, "%d", &v) != 1) v = -1; fclose(f); return v;
}
static void write_signal(int v) {
    FILE *f = fopen(g_ctrl_path, "w"); if (!f) return; fprintf(f, "%d", v); fclose(f);
}

static void dump_intercepted_info(void) {
    FILE *f = fopen(g_info_path, "w"); if (!f) return;
    pthread_mutex_lock(&g_lock);
    size_t n = atomic_load(&g_count), live = 0, bytes = 0;
    for (size_t i = 0; i < n; i++) if (g_allocs[i].live) { live++; bytes += g_allocs[i].size; }
    pthread_mutex_unlock(&g_lock);
    pthread_mutex_lock(&g_vmm_lock);
    size_t sn = atomic_load(&g_seg_n), vlive = 0, vbytes = 0;
    for (size_t i = 0; i < sn; i++) if (g_seg[i].live) { vlive++; vbytes += g_seg[i].size; }
    pthread_mutex_unlock(&g_vmm_lock);
    fprintf(f, "# GCR intercepted (pid=%d)\n", (int)getpid());
    fprintf(f, "legacy_live_buffers=%zu legacy_live_bytes=%zu\n", live, bytes);
    fprintf(f, "vmm_live_segments=%zu vmm_live_bytes=%zu\n", vlive, vbytes);
    fclose(f);
}

// ---- restore gate --------------------------------------------------------
// After a cross-container restore, the app process is resumed by `cuda-checkpoint
// unlock` while the interceptor still has to remap the GPU data buffers (recreate
// physical memory at the preserved VA + H2D). If the app runs a kernel before the
// remap finishes it touches an UNMAPPED virtual address and dies with
// CUDA_ERROR_INVALID_ARGUMENT / NO_DEVICE. We gate the app's kernel launches until
// restore_remap() completes. gcr_gate_wait() is on the launch hot path but is a
// single atomic load when the gate is open.
static atomic_int g_gate = 0;                       // 1 = app kernel launches blocked
static pthread_mutex_t g_gate_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t  g_gate_cv   = PTHREAD_COND_INITIALIZER;
static __thread int    g_in_remap  = 0;             // the remap thread must never gate itself

static void gcr_gate_set(int on) {
    pthread_mutex_lock(&g_gate_lock);
    atomic_store(&g_gate, on ? 1 : 0);
    if (!on) pthread_cond_broadcast(&g_gate_cv);
    pthread_mutex_unlock(&g_gate_lock);
}
static void gcr_gate_wait(void) {
    if (g_in_remap) return;                         // remap thread's own CUDA calls pass through
    if (!atomic_load(&g_gate)) return;              // fast path
    fprintf(stderr, "[gcr] gate: kernel launch held until data remap completes\n"); fflush(stderr);
    pthread_mutex_lock(&g_gate_lock);
    while (atomic_load(&g_gate)) pthread_cond_wait(&g_gate_cv, &g_gate_lock);
    pthread_mutex_unlock(&g_gate_lock);
    fprintf(stderr, "[gcr] gate: released, launch proceeds\n"); fflush(stderr);
}

static int gcr_vmm_enabled(void);
static size_t gcr_owned_count(void);
static void checkpoint_freeze(void);
static void restore_remap(void);

static void *watcher(void *arg) {
    (void)arg;
    for (;;) {
        int sig = read_signal();
        if (sig == GCR_CKPT) {
            fprintf(stderr, "[gcr] checkpoint signal received\n"); fflush(stderr);
            if (gcr_vmm_enabled() && gcr_owned_count() > 0) checkpoint_freeze();  // copy + free physical (keep VA)
            else vmm_dump();
            dump_intercepted_info();
            write_signal(GCR_IDLE);
            fprintf(stderr, "[gcr] checkpoint ACK sent\n"); fflush(stderr);
        } else if (sig == GCR_RESTORE) {
            fprintf(stderr, "[gcr] restore signal received\n"); fflush(stderr);
            if (gcr_vmm_enabled() && gcr_owned_count() > 0) {
                gcr_gate_set(1);            // (1) ensure gate up (already captured=1 from freeze under CRIUgpu)
                write_signal(GCR_GATING);  // (2) breadcrumb: gate is up, data remap starting
                restore_remap();           // (3) recreate physical + map same VA + H2D
                gcr_gate_set(0);           // (4) data valid -> release the gated app kernel launches
            }
            write_signal(GCR_IDLE);        // (5) done
            fprintf(stderr, "[gcr] restore ACK sent\n"); fflush(stderr);
        }
        usleep(50 * 1000);
    }
    return NULL;
}

__attribute__((constructor)) static void gcr_init(void) {
    ensure_real_dlsym();
    build_paths();
    pthread_t t;
    if (pthread_create(&t, NULL, watcher, NULL) == 0) pthread_detach(t);
    fprintf(stderr, "[gcr] interceptor loaded (pid=%d): ctrl=%s data=%s [VMM hooks active]\n",
            (int)getpid(), g_ctrl_path, g_data_dir);
    fflush(stderr);
}

// ---- GCR-owned VMM allocator (Step 2: back cudaMalloc with VMM) ----------
// When GCR_VMM_ALLOC=1, cudaMalloc is served from our own VMM mapping so that at
// checkpoint we can copy D2H, free ONLY the physical memory, and keep the virtual
// address (cuMemUnmap/cuMemRelease) — then remap to the same VA on resume.
#define CU_MEM_ALLOCATION_TYPE_PINNED      1
#define CU_MEM_LOCATION_TYPE_DEVICE        1
#define CU_MEM_ACCESS_FLAGS_PROT_READWRITE 3

typedef struct { struct { int type; int id; } location; int flags; } gcr_access_desc_t;

typedef struct { CUdeviceptr_t va; size_t padded; size_t req; CUmemHandle_t handle; gcr_mem_prop_t prop; int live; void *host_buf; size_t blob_off; int frozen; } gcr_owned_t;
static gcr_owned_t g_owned[GCR_MAX_VMM]; static atomic_size_t g_owned_n = 0;
static pthread_mutex_t g_owned_lock = PTHREAD_MUTEX_INITIALIZER;

static CUresult_t (*r_cuMemGetAllocationGranularity)(size_t *, const void *, int) = NULL;
static CUresult_t (*r_cuMemAddressReserve)(CUdeviceptr_t *, size_t, size_t, CUdeviceptr_t, unsigned long long) = NULL;
static CUresult_t (*r_cuMemCreate)(CUmemHandle_t *, size_t, const void *, unsigned long long) = NULL;
static CUresult_t (*r_cuMemMap)(CUdeviceptr_t, size_t, size_t, CUmemHandle_t, unsigned long long) = NULL;
static CUresult_t (*r_cuMemSetAccess)(CUdeviceptr_t, size_t, const void *, size_t) = NULL;
static CUresult_t (*r_cuMemUnmap2)(CUdeviceptr_t, size_t) = NULL;
static CUresult_t (*r_cuMemRelease2)(CUmemHandle_t) = NULL;
static CUresult_t (*r_cuMemAddressFree2)(CUdeviceptr_t, size_t) = NULL;
static int        (*r_cudaGetDevice)(int *) = NULL;

static int gcr_vmm_enabled(void) { const char *e = getenv("GCR_VMM_ALLOC"); return e && e[0] == '1'; }
static size_t gcr_owned_count(void) { return atomic_load(&g_owned_n); }

// Resolve a DRIVER (libcuda) symbol. libcuda is dlopen'd RTLD_LOCAL by cudart, so
// it is NOT in the global scope reachable via RTLD_NEXT — we must open a handle to
// libcuda.so.1 (already loaded; RTLD_NOLOAD returns it) and dlsym from it.
static void *gcr_cuda_sym(const char *name) {
    static void *libcuda = NULL;
    if (!libcuda) {
        libcuda = dlopen("libcuda.so.1", RTLD_NOLOAD | RTLD_LAZY);
        if (!libcuda) libcuda = dlopen("libcuda.so.1", RTLD_LAZY);
        if (!libcuda) libcuda = dlopen("libcuda.so", RTLD_LAZY);
        if (libcuda) { fprintf(stderr, "[gcr] libcuda handle acquired\n"); fflush(stderr); }
    }
    if (!libcuda) return NULL;
    ensure_real_dlsym();
    return real_dlsym ? real_dlsym(libcuda, name) : dlsym(libcuda, name);
}

static void resolve_vmm_real(void) {
    if (!r_cuMemGetAllocationGranularity) r_cuMemGetAllocationGranularity = (CUresult_t (*)(size_t *, const void *, int))gcr_cuda_sym("cuMemGetAllocationGranularity");
    if (!r_cuMemAddressReserve) r_cuMemAddressReserve = (CUresult_t (*)(CUdeviceptr_t *, size_t, size_t, CUdeviceptr_t, unsigned long long))gcr_cuda_sym("cuMemAddressReserve");
    if (!r_cuMemCreate)  r_cuMemCreate  = (CUresult_t (*)(CUmemHandle_t *, size_t, const void *, unsigned long long))gcr_cuda_sym("cuMemCreate");
    if (!r_cuMemMap)     r_cuMemMap     = (CUresult_t (*)(CUdeviceptr_t, size_t, size_t, CUmemHandle_t, unsigned long long))gcr_cuda_sym("cuMemMap");
    if (!r_cuMemSetAccess) r_cuMemSetAccess = (CUresult_t (*)(CUdeviceptr_t, size_t, const void *, size_t))gcr_cuda_sym("cuMemSetAccess");
    if (!r_cuMemUnmap2)  r_cuMemUnmap2  = (CUresult_t (*)(CUdeviceptr_t, size_t))gcr_cuda_sym("cuMemUnmap");
    if (!r_cuMemRelease2) r_cuMemRelease2 = (CUresult_t (*)(CUmemHandle_t))gcr_cuda_sym("cuMemRelease");
    if (!r_cuMemAddressFree2) r_cuMemAddressFree2 = (CUresult_t (*)(CUdeviceptr_t, size_t))gcr_cuda_sym("cuMemAddressFree");
    if (!r_cudaGetDevice) r_cudaGetDevice = (int (*)(int *))gcr_next("cudaGetDevice");
}

// Returns 0 on success (and sets *devPtr), -1 to fall back to the real cudaMalloc.
static int gcr_vmm_alloc(void **devPtr, size_t size) {
    resolve_vmm_real();
    if (!r_cuMemCreate || !r_cuMemMap || !r_cuMemAddressReserve || !r_cuMemSetAccess || !r_cuMemGetAllocationGranularity) {
        fprintf(stderr, "[gcr][vmm-alloc] unresolved syms: gran=%p reserve=%p create=%p map=%p setacc=%p getdev=%p\n",
                (void*)r_cuMemGetAllocationGranularity, (void*)r_cuMemAddressReserve, (void*)r_cuMemCreate,
                (void*)r_cuMemMap, (void*)r_cuMemSetAccess, (void*)r_cudaGetDevice);
        fflush(stderr);
        return -1;
    }
    int dev = 0; if (r_cudaGetDevice) r_cudaGetDevice(&dev);
    gcr_mem_prop_t prop; memset(&prop, 0, sizeof(prop));
    prop.type = CU_MEM_ALLOCATION_TYPE_PINNED;
    prop.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    prop.location.id = dev;
    size_t gran = 0;
    CUresult_t rg = r_cuMemGetAllocationGranularity(&gran, &prop, 0);
    if (rg != 0 || gran == 0) { fprintf(stderr, "[gcr][vmm-alloc] granularity rc=%d gran=%zu (dev=%d)\n", rg, gran, dev); if (gran == 0) gran = (2UL << 20); }
    size_t padded = ((size + gran - 1) / gran) * gran;
    if (padded == 0) padded = gran;
    CUmemHandle_t handle = 0;
    CUresult_t rc;
    if ((rc = r_cuMemCreate(&handle, padded, &prop, 0)) != 0) { fprintf(stderr, "[gcr][vmm-alloc] cuMemCreate rc=%d padded=%zu dev=%d\n", rc, padded, dev); fflush(stderr); return -1; }
    CUdeviceptr_t va = 0;
    if ((rc = r_cuMemAddressReserve(&va, padded, 0, 0, 0)) != 0) { fprintf(stderr, "[gcr][vmm-alloc] cuMemAddressReserve rc=%d\n", rc); fflush(stderr); r_cuMemRelease2(handle); return -1; }
    if ((rc = r_cuMemMap(va, padded, 0, handle, 0)) != 0) { fprintf(stderr, "[gcr][vmm-alloc] cuMemMap rc=%d\n", rc); fflush(stderr); r_cuMemAddressFree2(va, padded); r_cuMemRelease2(handle); return -1; }
    gcr_access_desc_t desc; memset(&desc, 0, sizeof(desc));
    desc.location.type = CU_MEM_LOCATION_TYPE_DEVICE; desc.location.id = dev;
    desc.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
    if ((rc = r_cuMemSetAccess(va, padded, &desc, 1)) != 0) { fprintf(stderr, "[gcr][vmm-alloc] cuMemSetAccess rc=%d\n", rc); fflush(stderr); r_cuMemUnmap2(va, padded); r_cuMemAddressFree2(va, padded); r_cuMemRelease2(handle); return -1; }
    pthread_mutex_lock(&g_owned_lock);
    size_t n = atomic_load(&g_owned_n);
    if (n < GCR_MAX_VMM) { g_owned[n].va = va; g_owned[n].padded = padded; g_owned[n].req = size; g_owned[n].handle = handle; g_owned[n].prop = prop; g_owned[n].live = 1; atomic_store(&g_owned_n, n + 1); }
    pthread_mutex_unlock(&g_owned_lock);
    *devPtr = (void *)(uintptr_t)va;
    fprintf(stderr, "[gcr][vmm-alloc] req=%zu padded=%zu va=0x%llx handle=%llu\n", size, padded, (unsigned long long)va, (unsigned long long)handle);
    fflush(stderr);
    return 0;
}

// Returns 0 if ptr was a GCR-owned VMM allocation (and freed it), -1 otherwise.
static int gcr_vmm_free(void *ptr) {
    CUdeviceptr_t va = (CUdeviceptr_t)(uintptr_t)ptr;
    pthread_mutex_lock(&g_owned_lock);
    size_t n = atomic_load(&g_owned_n); int found = -1;
    for (size_t i = 0; i < n; i++) if (g_owned[i].live && g_owned[i].va == va) { found = (int)i; break; }
    pthread_mutex_unlock(&g_owned_lock);
    if (found < 0) return -1;
    gcr_owned_t *o = &g_owned[found];
    if (r_cuMemUnmap2) r_cuMemUnmap2(o->va, o->padded);
    if (r_cuMemRelease2) r_cuMemRelease2(o->handle);
    if (r_cuMemAddressFree2) r_cuMemAddressFree2(o->va, o->padded);
    o->live = 0;
    fprintf(stderr, "[gcr][vmm-free] va=0x%llx\n", (unsigned long long)va);
    fflush(stderr);
    return 0;
}

// runtime fns for D2H/H2D copies (resolved lazily on the watcher thread)
static int (*rt_setdev)(int) = NULL;
static int (*rt_sync)(void) = NULL;
static int (*rt_memcpy)(void *, const void *, size_t, int) = NULL;
static void resolve_rt_copy(void) {
    if (!rt_setdev) rt_setdev = (int (*)(int))gcr_next("cudaSetDevice");
    if (!rt_sync)   rt_sync   = (int (*)(void))gcr_next("cudaDeviceSynchronize");
    if (!rt_memcpy) rt_memcpy = (int (*)(void *, const void *, size_t, int))gcr_next("cudaMemcpy");
}

// ---- external data blob (GCR-style: keep offloaded GPU data OUT of the CRIU image) ----
// GCR copies GPU data buffers into an external shared-memory file (their /mnt/huge),
// NOT into process-anonymous memory. CRIU records that as an external file mapping and
// does not serialize its content into the checkpoint tar. We mirror this: the offloaded
// buffers live in a MAP_SHARED file on GCR_DATA_DIR (a host bind-mount; put it on tmpfs
// for RAM speed). CRIUgpu then only checkpoints CPU + GPU control state.
#define ROUND2M(x) (((x) + ((2UL<<20)-1)) & ~((2UL<<20)-1))
#define GCR_STAGE_BYTES (128UL << 20)   // pinned staging chunk for fast PCIe D2H/H2D

static void  *g_blob = NULL;            // mmap base of the external data blob
static size_t g_blob_size = 0;
static int    g_blob_fd = -1;
static void  *g_stage = NULL;            // = g_stageA (serial fallback)
static void  *g_stageA = NULL, *g_stageB = NULL;   // double-buffered pinned staging
static int  (*rt_hostalloc)(void **, size_t, unsigned int) = NULL;  // cudaHostAlloc
static int  (*rt_hostfree)(void *) = NULL;                          // cudaFreeHost
static int  (*rt_memcpy_async)(void *, const void *, size_t, int, void *) = NULL; // cudaMemcpyAsync
static int  (*rt_stream_create)(void **) = NULL;
static int  (*rt_stream_destroy)(void *) = NULL;
static int  (*rt_stream_sync)(void *) = NULL;
static int  (*rt_event_create)(void **) = NULL;
static int  (*rt_event_destroy)(void *) = NULL;
static int  (*rt_event_record)(void *, void *) = NULL;
static int  (*rt_event_sync)(void *) = NULL;

static void gcr_stage_init(void) {
    if (g_stageA) return;
    if (!rt_hostalloc) rt_hostalloc = (int(*)(void**,size_t,unsigned int))gcr_next("cudaHostAlloc");
    if (!rt_hostfree)  rt_hostfree  = (int(*)(void*))gcr_next("cudaFreeHost");
    rt_memcpy_async   = (int(*)(void*,const void*,size_t,int,void*))gcr_next("cudaMemcpyAsync");
    rt_stream_create  = (int(*)(void**))gcr_next("cudaStreamCreate");
    rt_stream_destroy = (int(*)(void*))gcr_next("cudaStreamDestroy");
    rt_stream_sync    = (int(*)(void*))gcr_next("cudaStreamSynchronize");
    rt_event_create   = (int(*)(void**))gcr_next("cudaEventCreate");
    rt_event_destroy  = (int(*)(void*))gcr_next("cudaEventDestroy");
    rt_event_record   = (int(*)(void*,void*))gcr_next("cudaEventRecord");
    rt_event_sync     = (int(*)(void*))gcr_next("cudaEventSynchronize");
    if (rt_hostalloc && rt_hostalloc(&g_stageA, GCR_STAGE_BYTES, 0) == 0
                     && rt_hostalloc(&g_stageB, GCR_STAGE_BYTES, 0) == 0) {
        /* pinned double buffer -> async overlap path enabled */
    } else {
        g_stageA = malloc(GCR_STAGE_BYTES); g_stageB = malloc(GCR_STAGE_BYTES);
        rt_memcpy_async = NULL;   // no pinned host mem -> async is unsafe; force serial path
        fprintf(stderr, "[gcr][blob] pinned staging unavailable, using serial pageable copy\n");
    }
    g_stage = g_stageA;
}

// ---- overlapped copy engine (GCR-style: pinned double-buffer + async streams) ----
#define GCR_COPY_THREADS 4
typedef struct { char *dst; const char *src; size_t n; } gcr_mc_t;
static void *gcr_mc_worker(void *a) { gcr_mc_t *m = a; memcpy(m->dst, m->src, m->n); return NULL; }
static void memcpy_mt(void *dst, const void *src, size_t n) {  // multi-threaded host memcpy
    if (n < (8UL << 20)) { memcpy(dst, src, n); return; }
    pthread_t th[GCR_COPY_THREADS]; gcr_mc_t ar[GCR_COPY_THREADS];
    size_t per = (n + GCR_COPY_THREADS - 1) / GCR_COPY_THREADS; int t = 0;
    for (int i = 0; i < GCR_COPY_THREADS; i++) {
        size_t o = (size_t)i * per; if (o >= n) break;
        size_t c = (n - o < per) ? (n - o) : per;
        ar[i].dst = (char *)dst + o; ar[i].src = (const char *)src + o; ar[i].n = c;
        if (pthread_create(&th[t], NULL, gcr_mc_worker, &ar[i]) == 0) t++;
        else memcpy(ar[i].dst, ar[i].src, ar[i].n);
    }
    for (int i = 0; i < t; i++) pthread_join(th[i], NULL);
}

// Overlap PCIe with the host<->blob memcpy. mode 0 = freeze (D2H: dev->blob),
// mode 1 = remap (H2D: blob->dev). Returns 0 ok, -1 error, -2 async unavailable.
static int pipe_copy(int mode) {
    if (!rt_memcpy_async || !rt_stream_create || !rt_event_create || !rt_event_record
        || !rt_event_sync || !g_stageA || !g_stageB || !g_blob) return -2;
    void *stream = NULL;
    if (rt_stream_create(&stream) != 0 || !stream) return -2;
    void *ev[2] = { NULL, NULL };
    if (rt_event_create(&ev[0]) != 0 || rt_event_create(&ev[1]) != 0) {
        if (rt_stream_destroy) rt_stream_destroy(stream);
        return -2;
    }
    void *bufs[2] = { g_stageA, g_stageB };
    int prev = -1, j = 0, rc = 0;
    char *pbuf = NULL, *pblob = NULL; size_t psz = 0;
    size_t n = atomic_load(&g_owned_n);
    for (size_t i = 0; i < n; i++) {
        gcr_owned_t *o = &g_owned[i];
        int sel = (mode == 0) ? (o->live && !o->frozen) : (o->frozen);
        if (!sel) continue;
        size_t rem = o->req, coff = 0;
        while (rem > 0) {
            size_t chunk = rem < GCR_STAGE_BYTES ? rem : GCR_STAGE_BYTES;
            int b = j & 1; char *hbuf = (char *)bufs[b];
            void *dev = (void *)(uintptr_t)(o->va + coff);
            char *blob = (char *)g_blob + o->blob_off + coff;
            if (mode == 0) {
                if (rt_memcpy_async(hbuf, dev, chunk, 2 /*D2H*/, stream) != 0) { rc = -1; goto fin; }
            } else {
                memcpy_mt(hbuf, blob, chunk);
                if (rt_memcpy_async(dev, hbuf, chunk, 1 /*H2D*/, stream) != 0) { rc = -1; goto fin; }
            }
            rt_event_record(ev[b], stream);
            if (prev >= 0) {
                rt_event_sync(ev[prev & 1]);
                if (mode == 0) memcpy_mt(pblob, pbuf, psz);
            }
            prev = j; pbuf = hbuf; pblob = blob; psz = chunk;
            coff += chunk; rem -= chunk; j++;
        }
    }
    if (prev >= 0) {
        rt_event_sync(ev[prev & 1]);
        if (mode == 0) memcpy_mt(pblob, pbuf, psz);
    }
fin:
    if (rt_stream_sync) rt_stream_sync(stream);
    if (rt_event_destroy) { rt_event_destroy(ev[0]); rt_event_destroy(ev[1]); }
    if (rt_stream_destroy) rt_stream_destroy(stream);
    return rc;
}

// map (creating/growing) the external blob file to at least `need` bytes.
static int blob_map(size_t need) {
    if (!g_data_dir[0]) build_paths();
    if (g_blob && g_blob_size >= need) return 0;
    if (g_blob) { munmap(g_blob, g_blob_size); g_blob = NULL; }
    if (g_blob_fd < 0) {
        mkdir(g_data_dir, 0755);
        char pth[1200]; snprintf(pth, sizeof(pth), "%s/data.blob", g_data_dir);
        g_blob_fd = open(pth, O_CREAT|O_RDWR, 0644);
        if (g_blob_fd < 0) { fprintf(stderr, "[gcr][blob] open %s failed\n", pth); return -1; }
    }
    if (ftruncate(g_blob_fd, (off_t)need) != 0) { fprintf(stderr, "[gcr][blob] ftruncate failed\n"); return -1; }
    g_blob = mmap(NULL, need, PROT_READ|PROT_WRITE, MAP_SHARED, g_blob_fd, 0);
    if (g_blob == MAP_FAILED) { g_blob = NULL; fprintf(stderr, "[gcr][blob] mmap failed\n"); return -1; }
    g_blob_size = need;
    return 0;
}

// STEP 4a — checkpoint freeze: copy each owned buffer D2H into process memory,
// then free ONLY the physical memory (cuMemUnmap+cuMemRelease), KEEPING the VA.
// Leaves host_buf set + frozen=1 so CRIU captures the data and restore can remap.
static void checkpoint_freeze(void) {
    resolve_rt_copy(); resolve_vmm_real(); gcr_stage_init();
    if (!rt_memcpy || !r_cuMemUnmap2 || !r_cuMemRelease2) { fprintf(stderr, "[gcr][engine] freeze: missing fns\n"); fflush(stderr); return; }
    g_in_remap = 1;
    gcr_gate_set(1);
    if (rt_setdev) rt_setdev(0);
    if (rt_sync)   rt_sync();
    // size the blob + assign each segment its offset
    size_t n = atomic_load(&g_owned_n), total = 0;
    for (size_t i = 0; i < n; i++) if (g_owned[i].live && !g_owned[i].frozen) { g_owned[i].blob_off = total; total += ROUND2M(g_owned[i].req); }
    if (total == 0) { g_in_remap = 0; return; }
    if (blob_map(total) != 0) { fprintf(stderr, "[gcr][engine] freeze: blob map failed; aborting freeze\n"); fflush(stderr); g_in_remap = 0; return; }
    // copy device -> blob, overlapping PCIe with the host copy (fallback: serial)
    if (pipe_copy(0) == -2) {
        for (size_t i = 0; i < n; i++) {
            if (!g_owned[i].live || g_owned[i].frozen) continue;
            gcr_owned_t *o = &g_owned[i];
            size_t rem = o->req, coff = 0;
            while (rem > 0) {
                size_t chunk = rem < GCR_STAGE_BYTES ? rem : GCR_STAGE_BYTES;
                if (rt_memcpy(g_stage, (const void *)(uintptr_t)(o->va + coff), chunk, 2 /*D2H*/) != 0) break;
                memcpy_mt((char *)g_blob + o->blob_off + coff, g_stage, chunk);
                coff += chunk; rem -= chunk;
            }
        }
    }
    // release physical GPU memory (keep VA) for every offloaded segment
    size_t done = 0, bytes = 0;
    for (size_t i = 0; i < n; i++) {
        if (!g_owned[i].live || g_owned[i].frozen) continue;
        gcr_owned_t *o = &g_owned[i];
        r_cuMemUnmap2(o->va, o->padded);
        r_cuMemRelease2(o->handle);
        o->host_buf = NULL; o->frozen = 1;
        done++; bytes += o->req;
    }
    // Flush the blob to the backing file, then UNMAP + CLOSE it so that at CRIU-dump
    // time the process holds no mapping/fd to the data — CRIU cannot serialize it into
    // the tar. restore_remap() re-opens + re-maps the file to read the data back.
    if (g_blob) { msync(g_blob, g_blob_size, MS_ASYNC); munmap(g_blob, g_blob_size); g_blob = NULL; g_blob_size = 0; }  // ASYNC: don't block freeze on disk writeback (page cache holds the data for remap + the agent's copy)
    if (g_blob_fd >= 0) { close(g_blob_fd); g_blob_fd = -1; }
    fprintf(stderr, "[gcr][engine] freeze: %zu segs, %zu bytes -> external blob (overlapped copy; unmapped before checkpoint; excluded from CRIU tar); physical released (VA kept)\n", done, bytes);
    fflush(stderr);
    g_in_remap = 0;                     // gate stays armed (captured by CRIU); cleared on restore remap
}

// STEP 4b — restore remap: recreate physical, map to the SAME VA, copy host->device.
static void restore_remap(void) {
    g_in_remap = 1;
    resolve_rt_copy(); resolve_vmm_real();
    if (!rt_memcpy || !r_cuMemCreate || !r_cuMemMap || !r_cuMemSetAccess) { fprintf(stderr, "[gcr][engine] remap: missing fns\n"); fflush(stderr); return; }
    gcr_stage_init();
    if (!g_blob) {   // restored in a fresh process: re-map the external blob
        size_t need = 0, n0 = atomic_load(&g_owned_n);
        for (size_t i = 0; i < n0; i++) if (g_owned[i].frozen) { size_t e = g_owned[i].blob_off + ROUND2M(g_owned[i].req); if (e > need) need = e; }
        if (need) blob_map(need);
    }
    if (rt_setdev) rt_setdev(0);
    size_t n = atomic_load(&g_owned_n), done = 0, fail = 0;
    // Phase A: recreate physical + map SAME VA for each frozen segment
    for (size_t i = 0; i < n; i++) {
        if (!g_owned[i].frozen) continue;
        gcr_owned_t *o = &g_owned[i];
        CUmemHandle_t nh = 0;
        if (r_cuMemCreate(&nh, o->padded, &o->prop, 0) != 0) { fprintf(stderr, "[gcr][engine] remap recreate fail va=0x%llx\n", (unsigned long long)o->va); o->frozen = 0; fail++; continue; }
        if (r_cuMemMap(o->va, o->padded, 0, nh, 0) != 0) { fprintf(stderr, "[gcr][engine] remap map fail va=0x%llx\n", (unsigned long long)o->va); r_cuMemRelease2(nh); o->frozen = 0; fail++; continue; }
        gcr_access_desc_t d; memset(&d, 0, sizeof(d));
        d.location.type = CU_MEM_LOCATION_TYPE_DEVICE; d.location.id = o->prop.location.id;
        d.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
        r_cuMemSetAccess(o->va, o->padded, &d, 1);
        o->handle = nh;   // stays frozen=1 -> copied below, cleared in Phase C
    }
    // Phase B: copy blob -> device, overlapped async (fallback: serial)
    if (pipe_copy(1) == -2) {
        for (size_t i = 0; i < n; i++) {
            if (!g_owned[i].frozen) continue;
            gcr_owned_t *o = &g_owned[i];
            size_t rem = o->req, coff = 0;
            while (rem > 0 && g_blob) {
                size_t chunk = rem < GCR_STAGE_BYTES ? rem : GCR_STAGE_BYTES;
                memcpy_mt(g_stage, (char *)g_blob + o->blob_off + coff, chunk);
                rt_memcpy((void *)(uintptr_t)(o->va + coff), g_stage, chunk, 1 /*H2D*/);
                coff += chunk; rem -= chunk;
            }
        }
    }
    if (rt_sync) rt_sync();
    // Phase C: mark restored
    for (size_t i = 0; i < n; i++) if (g_owned[i].frozen) { g_owned[i].frozen = 0; done++; }
    fprintf(stderr, "[gcr][engine] remap: %zu segs restored from external blob to same VA + H2D; %zu failed\n", done, fail);
    fflush(stderr);
    g_in_remap = 0;
}

// ---- legacy intercepted CUDA memory APIs --------------------------------
static int (*real_cudaMalloc)(void **, size_t) = NULL;
int cudaMalloc(void **devPtr, size_t size) {
    if (gcr_vmm_enabled() && devPtr && size) {
        if (gcr_vmm_alloc(devPtr, size) == 0) return 0;  // cudaSuccess, VMM-backed
        fprintf(stderr, "[gcr][vmm-alloc] FAILED for size=%zu, falling back to real cudaMalloc\n", size); fflush(stderr);
    }
    if (!real_cudaMalloc) real_cudaMalloc = (int (*)(void **, size_t))gcr_next("cudaMalloc");
    int rc = real_cudaMalloc(devPtr, size);
    if (rc == 0 && devPtr) { reg_add(*devPtr, size); fprintf(stderr, "[gcr][rt] cudaMalloc size=%zu ptr=%p\n", size, *devPtr); fflush(stderr); }
    return rc;
}
static int (*real_cudaFree)(void *) = NULL;
int cudaFree(void *devPtr) {
    if (devPtr && gcr_vmm_free(devPtr) == 0) return 0;  // was a GCR-owned VMM allocation
    if (!real_cudaFree) real_cudaFree = (int (*)(void *))gcr_next("cudaFree");
    reg_del(devPtr);
    return real_cudaFree(devPtr);
}
static int (*real_cuMemAlloc)(unsigned long long *, size_t) = NULL;
int cuMemAlloc_v2(unsigned long long *dptr, size_t bytesize) {
    if (!real_cuMemAlloc) real_cuMemAlloc = (int (*)(unsigned long long *, size_t))gcr_next("cuMemAlloc_v2");
    int rc = real_cuMemAlloc(dptr, bytesize);
    if (rc == 0 && dptr) reg_add((void *)(uintptr_t)(*dptr), bytesize);
    return rc;
}
static int (*real_cuMemFree)(unsigned long long) = NULL;
int cuMemFree_v2(unsigned long long dptr) {
    if (!real_cuMemFree) real_cuMemFree = (int (*)(unsigned long long))gcr_next("cuMemFree_v2");
    reg_del((void *)(uintptr_t)dptr);
    return real_cuMemFree(dptr);
}

// stream-ordered allocations (PyTorch may use these instead of cudaMalloc)
static int (*real_cudaMallocAsync)(void **, size_t, void *) = NULL;
int cudaMallocAsync(void **devPtr, size_t size, void *stream) {
    if (!real_cudaMallocAsync) real_cudaMallocAsync = (int (*)(void **, size_t, void *))gcr_next("cudaMallocAsync");
    int rc = real_cudaMallocAsync(devPtr, size, stream);
    if (rc == 0 && devPtr) { reg_add(*devPtr, size); fprintf(stderr, "[gcr][rt] cudaMallocAsync size=%zu ptr=%p\n", size, *devPtr); fflush(stderr); }
    return rc;
}
static int (*real_cudaFreeAsync)(void *, void *) = NULL;
int cudaFreeAsync(void *devPtr, void *stream) {
    if (!real_cudaFreeAsync) real_cudaFreeAsync = (int (*)(void *, void *))gcr_next("cudaFreeAsync");
    reg_del(devPtr);
    return real_cudaFreeAsync(devPtr, stream);
}

// ---- VMM intercepted APIs (Level 1, step 1: observe) --------------------
static CUresult_t (*real_cuMemCreate)(CUmemHandle_t *, size_t, const void *, unsigned long long) = NULL;
CUresult_t cuMemCreate(CUmemHandle_t *handle, size_t size, const void *prop, unsigned long long flags) {
    if (!real_cuMemCreate) real_cuMemCreate = (CUresult_t (*)(CUmemHandle_t *, size_t, const void *, unsigned long long))gcr_next("cuMemCreate");
    CUresult_t rc = real_cuMemCreate(handle, size, prop, flags);
    if (rc == 0 && handle) vmm_phys_add(*handle, size, prop);
    return rc;
}
static CUresult_t (*real_cuMemMap)(CUdeviceptr_t, size_t, size_t, CUmemHandle_t, unsigned long long) = NULL;
CUresult_t cuMemMap(CUdeviceptr_t ptr, size_t size, size_t offset, CUmemHandle_t handle, unsigned long long flags) {
    if (!real_cuMemMap) real_cuMemMap = (CUresult_t (*)(CUdeviceptr_t, size_t, size_t, CUmemHandle_t, unsigned long long))gcr_next("cuMemMap");
    CUresult_t rc = real_cuMemMap(ptr, size, offset, handle, flags);
    if (rc == 0) vmm_seg_add(ptr, size, offset, handle);
    return rc;
}
static CUresult_t (*real_cuMemUnmap)(CUdeviceptr_t, size_t) = NULL;
CUresult_t cuMemUnmap(CUdeviceptr_t ptr, size_t size) {
    if (!real_cuMemUnmap) real_cuMemUnmap = (CUresult_t (*)(CUdeviceptr_t, size_t))gcr_next("cuMemUnmap");
    vmm_seg_del(ptr);
    return real_cuMemUnmap(ptr, size);
}
static CUresult_t (*real_cuMemRelease)(CUmemHandle_t) = NULL;
CUresult_t cuMemRelease(CUmemHandle_t handle) {
    if (!real_cuMemRelease) real_cuMemRelease = (CUresult_t (*)(CUmemHandle_t))gcr_next("cuMemRelease");
    vmm_phys_del(handle);
    return real_cuMemRelease(handle);
}
static CUresult_t (*real_cuMemAddressReserve)(CUdeviceptr_t *, size_t, size_t, CUdeviceptr_t, unsigned long long) = NULL;
CUresult_t cuMemAddressReserve(CUdeviceptr_t *ptr, size_t size, size_t alignment, CUdeviceptr_t addr, unsigned long long flags) {
    if (!real_cuMemAddressReserve) real_cuMemAddressReserve = (CUresult_t (*)(CUdeviceptr_t *, size_t, size_t, CUdeviceptr_t, unsigned long long))gcr_next("cuMemAddressReserve");
    CUresult_t rc = real_cuMemAddressReserve(ptr, size, alignment, addr, flags);
    if (rc == 0 && ptr) { fprintf(stderr, "[gcr][vmm] cuMemAddressReserve va=0x%llx size=%zu\n", (unsigned long long)*ptr, size); fflush(stderr); }
    return rc;
}
static CUresult_t (*real_cuMemAddressFree)(CUdeviceptr_t, size_t) = NULL;
CUresult_t cuMemAddressFree(CUdeviceptr_t ptr, size_t size) {
    if (!real_cuMemAddressFree) real_cuMemAddressFree = (CUresult_t (*)(CUdeviceptr_t, size_t))gcr_next("cuMemAddressFree");
    return real_cuMemAddressFree(ptr, size);
}

// ---- kernel-launch hooks: gate app compute during restore remap ----------
// The app must not launch a kernel that reads the frozen data until restore_remap
// has re-mapped it. We interpose the driver and runtime launch entrypoints and
// block on the restore gate first (a no-op when the gate is open).
typedef struct { unsigned int x, y, z; } gcr_dim3;

static CUresult_t (*real_cuLaunchKernel)(void *, unsigned, unsigned, unsigned,
                                         unsigned, unsigned, unsigned, unsigned,
                                         void *, void **, void **) = NULL;
CUresult_t cuLaunchKernel(void *f, unsigned gx, unsigned gy, unsigned gz,
                          unsigned bx, unsigned by, unsigned bz,
                          unsigned shmem, void *stream, void **kparams, void **extra) {
    gcr_gate_wait();
    if (!real_cuLaunchKernel) {
        real_cuLaunchKernel = (CUresult_t (*)(void *, unsigned, unsigned, unsigned, unsigned, unsigned, unsigned, unsigned, void *, void **, void **))gcr_cuda_sym("cuLaunchKernel");
        if (!real_cuLaunchKernel) real_cuLaunchKernel = (CUresult_t (*)(void *, unsigned, unsigned, unsigned, unsigned, unsigned, unsigned, unsigned, void *, void **, void **))gcr_next("cuLaunchKernel");
    }
    return real_cuLaunchKernel(f, gx, gy, gz, bx, by, bz, shmem, stream, kparams, extra);
}

static int (*real_cudaLaunchKernel)(const void *, gcr_dim3, gcr_dim3, void **, size_t, void *) = NULL;
int cudaLaunchKernel(const void *func, gcr_dim3 gridDim, gcr_dim3 blockDim, void **args, size_t sharedMem, void *stream) {
    gcr_gate_wait();
    if (!real_cudaLaunchKernel) real_cudaLaunchKernel = (int (*)(const void *, gcr_dim3, gcr_dim3, void **, size_t, void *))gcr_next("cudaLaunchKernel");
    return real_cudaLaunchKernel(func, gridDim, blockDim, args, sharedMem, stream);
}

// ---- CUDA 12 / cooperative launch entry points (also gated) --------------
// PyTorch 2.4 + CUDA 12 dispatch kernels through cuLaunchKernelEx, not just
// cuLaunchKernel; cover the additional launch paths so the restore gate holds
// every kernel until the data buffers are remapped.
static CUresult_t (*real_cuLaunchKernelEx)(const void *, void *, void **, void **) = NULL;
CUresult_t cuLaunchKernelEx(const void *config, void *f, void **kernelParams, void **extra) {
    gcr_gate_wait();
    if (!real_cuLaunchKernelEx) {
        real_cuLaunchKernelEx = (CUresult_t (*)(const void *, void *, void **, void **))gcr_cuda_sym("cuLaunchKernelEx");
        if (!real_cuLaunchKernelEx) real_cuLaunchKernelEx = (CUresult_t (*)(const void *, void *, void **, void **))gcr_next("cuLaunchKernelEx");
    }
    return real_cuLaunchKernelEx(config, f, kernelParams, extra);
}

static CUresult_t (*real_cuLaunchCooperativeKernel)(void *, unsigned, unsigned, unsigned,
                                                    unsigned, unsigned, unsigned, unsigned,
                                                    void *, void **) = NULL;
CUresult_t cuLaunchCooperativeKernel(void *f, unsigned gx, unsigned gy, unsigned gz,
                                     unsigned bx, unsigned by, unsigned bz,
                                     unsigned shmem, void *stream, void **kparams) {
    gcr_gate_wait();
    if (!real_cuLaunchCooperativeKernel) {
        real_cuLaunchCooperativeKernel = (CUresult_t (*)(void *, unsigned, unsigned, unsigned, unsigned, unsigned, unsigned, unsigned, void *, void **))gcr_cuda_sym("cuLaunchCooperativeKernel");
        if (!real_cuLaunchCooperativeKernel) real_cuLaunchCooperativeKernel = (CUresult_t (*)(void *, unsigned, unsigned, unsigned, unsigned, unsigned, unsigned, unsigned, void *, void **))gcr_next("cuLaunchCooperativeKernel");
    }
    return real_cuLaunchCooperativeKernel(f, gx, gy, gz, bx, by, bz, shmem, stream, kparams);
}

// runtime CUDA 12 extended launch (struct-pointer + pointer args -> ABI-safe)
static int (*real_cudaLaunchKernelExC)(const void *, const void *, void **) = NULL;
int cudaLaunchKernelExC(const void *config, const void *func, void **args) {
    gcr_gate_wait();
    if (!real_cudaLaunchKernelExC) real_cudaLaunchKernelExC = (int (*)(const void *, const void *, void **))gcr_next("cudaLaunchKernelExC");
    return real_cudaLaunchKernelExC(config, func, args);
}
