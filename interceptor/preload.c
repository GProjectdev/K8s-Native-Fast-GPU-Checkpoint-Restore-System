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

#define GCR_IDLE 0
#define GCR_CKPT 1
#define GCR_RESTORE 2

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

static void *watcher(void *arg) {
    (void)arg;
    for (;;) {
        if (read_signal() == GCR_CKPT) {
            fprintf(stderr, "[gcr] checkpoint signal received (STEP 1: observe VMM, no copy)\n");
            fflush(stderr);
            vmm_dump();              // <-- the new Level-1 observation
            dump_intercepted_info();
            write_signal(GCR_IDLE);
            fprintf(stderr, "[gcr] checkpoint ACK sent\n");
            fflush(stderr);
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

typedef struct { CUdeviceptr_t va; size_t padded; size_t req; CUmemHandle_t handle; gcr_mem_prop_t prop; int live; } gcr_owned_t;
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

static void resolve_vmm_real(void) {
    if (!r_cuMemGetAllocationGranularity) r_cuMemGetAllocationGranularity = (CUresult_t (*)(size_t *, const void *, int))gcr_next("cuMemGetAllocationGranularity");
    if (!r_cuMemAddressReserve) r_cuMemAddressReserve = (CUresult_t (*)(CUdeviceptr_t *, size_t, size_t, CUdeviceptr_t, unsigned long long))gcr_next("cuMemAddressReserve");
    if (!r_cuMemCreate)  r_cuMemCreate  = (CUresult_t (*)(CUmemHandle_t *, size_t, const void *, unsigned long long))gcr_next("cuMemCreate");
    if (!r_cuMemMap)     r_cuMemMap     = (CUresult_t (*)(CUdeviceptr_t, size_t, size_t, CUmemHandle_t, unsigned long long))gcr_next("cuMemMap");
    if (!r_cuMemSetAccess) r_cuMemSetAccess = (CUresult_t (*)(CUdeviceptr_t, size_t, const void *, size_t))gcr_next("cuMemSetAccess");
    if (!r_cuMemUnmap2)  r_cuMemUnmap2  = (CUresult_t (*)(CUdeviceptr_t, size_t))gcr_next("cuMemUnmap");
    if (!r_cuMemRelease2) r_cuMemRelease2 = (CUresult_t (*)(CUmemHandle_t))gcr_next("cuMemRelease");
    if (!r_cuMemAddressFree2) r_cuMemAddressFree2 = (CUresult_t (*)(CUdeviceptr_t, size_t))gcr_next("cuMemAddressFree");
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
