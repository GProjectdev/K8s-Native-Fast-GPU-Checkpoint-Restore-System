// libgcr-interceptor.so — selective CUDA-interception shim (LD_PRELOAD).
//
// This realizes the GCR *idea* (selective interception-based data tracking +
// control/data separation) with our OWN code — it does NOT depend on the
// upstream GCR hook driver.
//
// What it does, inside a GPU Pod that LD_PRELOADs it:
//   1. Interposes the CUDA memory-management APIs (cudaMalloc/cudaFree and the
//      driver cuMemAlloc_v2/cuMemFree_v2) to keep a live registry of GPU buffers
//      (the "selective interception" — we only hook memory APIs, ~0 overhead on
//      everything else).
//   2. Runs a background watcher thread that polls a control channel shared with
//      the GPU C/R Node Agent. When the agent requests a checkpoint (writes "1"),
//      the shim dumps the intercepted buffer info ("Intercepted info file" in the
//      DCN Progress Report) and ACKs by writing "0" — so the agent can proceed to
//      the driver-integrated control-state checkpoint (cuda-checkpoint) + CRIU.
//
// No CUDA headers are needed — we match the stable CUDA ABI directly.

#define _GNU_SOURCE
#include <dlfcn.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

// ---- GCR control signals (match internal/agent/interceptor.go) ----
#define GCR_IDLE 0
#define GCR_CKPT 1
#define GCR_RESTORE 2

// ---- GPU allocation registry --------------------------------------------
typedef struct {
    void *ptr;
    size_t size;
    int live;
} gcr_alloc_t;

#define GCR_MAX_ALLOCS 65536
static gcr_alloc_t g_allocs[GCR_MAX_ALLOCS];
static atomic_size_t g_count = 0;
static atomic_size_t g_live_bytes = 0;
static pthread_mutex_t g_lock = PTHREAD_MUTEX_INITIALIZER;

static void reg_add(void *ptr, size_t size) {
    if (ptr == NULL) return;
    pthread_mutex_lock(&g_lock);
    size_t n = atomic_load(&g_count);
    if (n < GCR_MAX_ALLOCS) {
        g_allocs[n].ptr = ptr;
        g_allocs[n].size = size;
        g_allocs[n].live = 1;
        atomic_store(&g_count, n + 1);
        atomic_fetch_add(&g_live_bytes, size);
    }
    pthread_mutex_unlock(&g_lock);
}

static void reg_del(void *ptr) {
    if (ptr == NULL) return;
    pthread_mutex_lock(&g_lock);
    size_t n = atomic_load(&g_count);
    for (size_t i = 0; i < n; i++) {
        if (g_allocs[i].live && g_allocs[i].ptr == ptr) {
            g_allocs[i].live = 0;
            atomic_fetch_sub(&g_live_bytes, g_allocs[i].size);
            break;
        }
    }
    pthread_mutex_unlock(&g_lock);
}

// ---- control channel ----------------------------------------------------
static char g_ctrl_path[1024];
static char g_info_path[1024];

static void build_paths(void) {
    const char *dir = getenv("GCR_CONTROL_DIR");
    if (!dir || !*dir) dir = "/var/lib/gpu-cr/run";
    const char *uid = getenv("GCR_POD_UID");
    if (!uid || !*uid) uid = "default";
    snprintf(g_ctrl_path, sizeof(g_ctrl_path), "%s/%s/control", dir, uid);
    snprintf(g_info_path, sizeof(g_info_path), "%s/%s/intercepted-info", dir, uid);
}

static int read_signal(void) {
    FILE *f = fopen(g_ctrl_path, "r");
    if (!f) return -1;
    int v = -1;
    if (fscanf(f, "%d", &v) != 1) v = -1;
    fclose(f);
    return v;
}

static void write_signal(int v) {
    FILE *f = fopen(g_ctrl_path, "w");
    if (!f) return;
    fprintf(f, "%d", v);
    fclose(f);
}

// Dump the intercepted GPU buffer registry — the "intercepted info" the agent
// can read (selective-interception product for control/data separation).
static void dump_intercepted_info(void) {
    FILE *f = fopen(g_info_path, "w");
    if (!f) return;
    pthread_mutex_lock(&g_lock);
    size_t n = atomic_load(&g_count);
    size_t live = 0, bytes = 0;
    fprintf(f, "# GCR intercepted GPU buffers (pid=%d)\n", (int)getpid());
    fprintf(f, "# ptr size_bytes\n");
    for (size_t i = 0; i < n; i++) {
        if (g_allocs[i].live) {
            fprintf(f, "%p %zu\n", g_allocs[i].ptr, g_allocs[i].size);
            live++;
            bytes += g_allocs[i].size;
        }
    }
    fprintf(f, "# live_buffers=%zu live_bytes=%zu\n", live, bytes);
    pthread_mutex_unlock(&g_lock);
    fclose(f);
    fprintf(stderr, "[gcr] intercepted-info dumped: %zu live buffers, %zu bytes -> %s\n",
            live, bytes, g_info_path);
    fflush(stderr);
}

static void *watcher(void *arg) {
    (void)arg;
    for (;;) {
        int sig = read_signal();
        if (sig == GCR_CKPT) {
            fprintf(stderr, "[gcr] checkpoint signal received; selective data-buffer "
                            "checkpoint (intercepted info)\n");
            fflush(stderr);
            dump_intercepted_info();
            write_signal(GCR_IDLE);
            fprintf(stderr, "[gcr] data-buffer checkpoint ACK sent\n");
            fflush(stderr);
        }
        usleep(50 * 1000);
    }
    return NULL;
}

__attribute__((constructor)) static void gcr_init(void) {
    build_paths();
    pthread_t t;
    if (pthread_create(&t, NULL, watcher, NULL) == 0) {
        pthread_detach(t);
    }
    fprintf(stderr, "[gcr] interceptor loaded (pid=%d): watching %s\n",
            (int)getpid(), g_ctrl_path);
    fflush(stderr);
}

// ---- intercepted CUDA memory APIs ---------------------------------------
static int (*real_cudaMalloc)(void **, size_t) = NULL;
int cudaMalloc(void **devPtr, size_t size) {
    if (!real_cudaMalloc)
        real_cudaMalloc = (int (*)(void **, size_t))dlsym(RTLD_NEXT, "cudaMalloc");
    int rc = real_cudaMalloc(devPtr, size);
    if (rc == 0 && devPtr) reg_add(*devPtr, size);
    return rc;
}

static int (*real_cudaFree)(void *) = NULL;
int cudaFree(void *devPtr) {
    if (!real_cudaFree)
        real_cudaFree = (int (*)(void *))dlsym(RTLD_NEXT, "cudaFree");
    reg_del(devPtr);
    return real_cudaFree(devPtr);
}

static int (*real_cuMemAlloc)(unsigned long long *, size_t) = NULL;
int cuMemAlloc_v2(unsigned long long *dptr, size_t bytesize) {
    if (!real_cuMemAlloc)
        real_cuMemAlloc = (int (*)(unsigned long long *, size_t))dlsym(RTLD_NEXT, "cuMemAlloc_v2");
    int rc = real_cuMemAlloc(dptr, bytesize);
    if (rc == 0 && dptr) reg_add((void *)(uintptr_t)(*dptr), bytesize);
    return rc;
}

static int (*real_cuMemFree)(unsigned long long) = NULL;
int cuMemFree_v2(unsigned long long dptr) {
    if (!real_cuMemFree)
        real_cuMemFree = (int (*)(unsigned long long))dlsym(RTLD_NEXT, "cuMemFree_v2");
    reg_del((void *)(uintptr_t)dptr);
    return real_cuMemFree(dptr);
}
