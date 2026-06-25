// libgcr-interceptor.so — selective CUDA-interception shim (LD_PRELOAD).
//
// Realizes the GCR *idea* with our own code (no upstream GCR hook):
//   1. Interposes CUDA memory APIs (cudaMalloc/cudaFree, cuMemAlloc/Free) to keep
//      a live registry of GPU buffers — the "selective interception".
//   2. On the Node Agent's checkpoint signal, performs the SELECTIVE DATA-BUFFER
//      CHECKPOINT: actually copies each tracked GPU buffer Device->Host (chunked
//      cudaMemcpy) into the checkpoint storage, writes the intercepted-info, and
//      ACKs. Best-effort: if the in-process copy fails it logs and still ACKs, so
//      the agent proceeds (cuda-checkpoint + CRIU then handle the rest).
//
// No CUDA headers needed — we match the stable CUDA ABI directly.

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
#define CUDA_MEMCPY_D2H 2
#define CHUNK (64UL << 20) // 64MB host staging chunk

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

// lazily-resolved CUDA runtime fns for the D2H copy
static int (*real_cudaSetDevice)(int) = NULL;
static int (*real_cudaDeviceSynchronize)(void) = NULL;
static int (*real_cudaMemcpy)(void *, const void *, size_t, int) = NULL;

static void resolve_rt(void) {
    if (!real_cudaSetDevice)        real_cudaSetDevice        = (int (*)(int))dlsym(RTLD_NEXT, "cudaSetDevice");
    if (!real_cudaDeviceSynchronize)real_cudaDeviceSynchronize= (int (*)(void))dlsym(RTLD_NEXT, "cudaDeviceSynchronize");
    if (!real_cudaMemcpy)           real_cudaMemcpy           = (int (*)(void *, const void *, size_t, int))dlsym(RTLD_NEXT, "cudaMemcpy");
}

// SELECTIVE DATA-BUFFER CHECKPOINT: copy tracked GPU buffers D->H into storage.
// Returns total bytes copied, or -1 if the copy path is unavailable.
static long long checkpoint_data_buffers(void) {
    resolve_rt();
    if (!real_cudaMemcpy || !real_cudaDeviceSynchronize) {
        fprintf(stderr, "[gcr] cudaMemcpy not resolvable; skipping in-process data copy\n");
        return -1;
    }
    mkdir(g_data_dir, 0755);
    char path[1100];
    snprintf(path, sizeof(path), "%s/data-checkpoint.bin", g_data_dir);
    FILE *out = fopen(path, "wb");
    if (!out) { fprintf(stderr, "[gcr] cannot open %s\n", path); return -1; }

    if (real_cudaSetDevice) real_cudaSetDevice(0);
    real_cudaDeviceSynchronize();

    void *host = malloc(CHUNK);
    if (!host) { fclose(out); return -1; }

    long long total = 0; size_t nbuf = 0;
    pthread_mutex_lock(&g_lock);
    size_t n = atomic_load(&g_count);
    for (size_t i = 0; i < n; i++) {
        if (!g_allocs[i].live) continue;
        void *dptr = g_allocs[i].ptr; size_t size = g_allocs[i].size;
        // record: [ptr(8)][size(8)] then raw bytes
        uint64_t hp = (uint64_t)(uintptr_t)dptr, hs = (uint64_t)size;
        fwrite(&hp, sizeof(hp), 1, out); fwrite(&hs, sizeof(hs), 1, out);
        size_t off = 0; int ok = 1;
        while (off < size) {
            size_t c = (size - off < CHUNK) ? (size - off) : CHUNK;
            int rc = real_cudaMemcpy(host, (const char *)dptr + off, c, CUDA_MEMCPY_D2H);
            if (rc != 0) { fprintf(stderr, "[gcr] cudaMemcpy D2H failed (rc=%d) for %p\n", rc, dptr); ok = 0; break; }
            fwrite(host, 1, c, out);
            off += c;
        }
        if (ok) { total += (long long)size; nbuf++; }
    }
    pthread_mutex_unlock(&g_lock);
    free(host); fclose(out);
    fprintf(stderr, "[gcr] selective data-buffer checkpoint: %zu buffers, %lld bytes -> %s\n",
            nbuf, total, path);
    return total;
}

static void dump_intercepted_info(long long copied) {
    FILE *f = fopen(g_info_path, "w"); if (!f) return;
    pthread_mutex_lock(&g_lock);
    size_t n = atomic_load(&g_count), live = 0, bytes = 0;
    fprintf(f, "# GCR intercepted GPU buffers (pid=%d)\n# ptr size_bytes\n", (int)getpid());
    for (size_t i = 0; i < n; i++) if (g_allocs[i].live) { fprintf(f, "%p %zu\n", g_allocs[i].ptr, g_allocs[i].size); live++; bytes += g_allocs[i].size; }
    fprintf(f, "# live_buffers=%zu live_bytes=%zu copied_bytes=%lld\n", live, bytes, copied);
    pthread_mutex_unlock(&g_lock);
    fclose(f);
}

static void *watcher(void *arg) {
    (void)arg;
    for (;;) {
        if (read_signal() == GCR_CKPT) {
            fprintf(stderr, "[gcr] checkpoint signal received; selective data-buffer checkpoint\n");
            fflush(stderr);
            long long copied = checkpoint_data_buffers();
            dump_intercepted_info(copied);
            write_signal(GCR_IDLE);
            fprintf(stderr, "[gcr] data-buffer checkpoint ACK sent (copied=%lld bytes)\n", copied);
            fflush(stderr);
        }
        usleep(50 * 1000);
    }
    return NULL;
}

__attribute__((constructor)) static void gcr_init(void) {
    build_paths();
    pthread_t t;
    if (pthread_create(&t, NULL, watcher, NULL) == 0) pthread_detach(t);
    fprintf(stderr, "[gcr] interceptor loaded (pid=%d): ctrl=%s data=%s\n",
            (int)getpid(), g_ctrl_path, g_data_dir);
    fflush(stderr);
}

// ---- intercepted CUDA memory APIs ----
static int (*real_cudaMalloc)(void **, size_t) = NULL;
int cudaMalloc(void **devPtr, size_t size) {
    if (!real_cudaMalloc) real_cudaMalloc = (int (*)(void **, size_t))dlsym(RTLD_NEXT, "cudaMalloc");
    int rc = real_cudaMalloc(devPtr, size);
    if (rc == 0 && devPtr) reg_add(*devPtr, size);
    return rc;
}
static int (*real_cudaFree)(void *) = NULL;
int cudaFree(void *devPtr) {
    if (!real_cudaFree) real_cudaFree = (int (*)(void *))dlsym(RTLD_NEXT, "cudaFree");
    reg_del(devPtr);
    return real_cudaFree(devPtr);
}
static int (*real_cuMemAlloc)(unsigned long long *, size_t) = NULL;
int cuMemAlloc_v2(unsigned long long *dptr, size_t bytesize) {
    if (!real_cuMemAlloc) real_cuMemAlloc = (int (*)(unsigned long long *, size_t))dlsym(RTLD_NEXT, "cuMemAlloc_v2");
    int rc = real_cuMemAlloc(dptr, bytesize);
    if (rc == 0 && dptr) reg_add((void *)(uintptr_t)(*dptr), bytesize);
    return rc;
}
static int (*real_cuMemFree)(unsigned long long) = NULL;
int cuMemFree_v2(unsigned long long dptr) {
    if (!real_cuMemFree) real_cuMemFree = (int (*)(unsigned long long))dlsym(RTLD_NEXT, "cuMemFree_v2");
    reg_del((void *)(uintptr_t)dptr);
    return real_cuMemFree(dptr);
}
