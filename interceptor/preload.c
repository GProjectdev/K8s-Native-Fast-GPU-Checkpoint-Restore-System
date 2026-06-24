// libgcr-interceptor.so  -- selective CUDA interception shim (LD_PRELOAD).
//
// Adapted from thustorage/GCR (GCR/preload.c), itself derived from
// open-neutrino. This shim is the piece a GPU Pod LD_PRELOADs:
//
//   env:
//     - name: LD_PRELOAD
//       value: /opt/gpu-cr/libgcr-interceptor.so
//     - name: GCR_HOME
//       value: /opt/gpu-cr            # dir containing the GCR hook libcuda.so
//
// It hooks dlopen so that when the CUDA runtime (or the app) loads
// "libcuda.so.1", it transparently receives GCR's hook driver instead
// ($GCR_HOME/libcuda.so), which performs *selective* interception of only the
// GPU memory-management APIs (cuMemCreate/Map/Unmap/Release) -- the control/data
// separation at the heart of GCR. All other driver calls fall through to the
// real driver, so normal-execution overhead stays < 1%.
//
// Calls originating from libcublas are routed to the real driver unchanged
// (cublas dlopen's libcuda for its own internal handles).

#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdio.h>
#include <string.h>
#include <execinfo.h>
#include <stdlib.h>

#ifndef STACK_TRACE_SIZE
#define STACK_TRACE_SIZE 8
#endif

#define CHECK_DL()                                  \
    do {                                            \
        const char *dl_error = dlerror();           \
        if (dl_error) {                             \
            fprintf(stderr, "[gcr] %s\n", dl_error);\
        }                                           \
    } while (0)

static void *(*real_dlopen)(const char *filename, int flags) = NULL;

// Real CUDA driver location (override with GCR_REAL_CUDA).
static const char *default_real_cuda = "/usr/lib/x86_64-linux-gnu/libcuda.so.1";
static char real_cuda[512];

// GCR hook driver path, derived from $GCR_HOME at load time.
static char hook_driver[512];
static const char *driver_name = "libcuda.so";

__attribute__((constructor)) static void preload_init(void) {
    const char *home = getenv("GCR_HOME");
    if (home == NULL) home = "/opt/gpu-cr";
    snprintf(hook_driver, sizeof(hook_driver), "%s/libcuda.so", home);

    const char *rc = getenv("GCR_REAL_CUDA");
    snprintf(real_cuda, sizeof(real_cuda), "%s", rc ? rc : default_real_cuda);

    fprintf(stderr, "[gcr] interceptor loaded: hook=%s real=%s\n",
            hook_driver, real_cuda);
}

void *dlopen(const char *filename, int flags) {
    if (!real_dlopen)
        real_dlopen = dlsym(RTLD_NEXT, "dlopen");

    if (filename != NULL && strstr(filename, driver_name) != NULL) {
        // Determine whether this dlopen comes from cublas.
        void *frames[STACK_TRACE_SIZE];
        int n = backtrace(frames, STACK_TRACE_SIZE);
        char **syms = backtrace_symbols(frames, n);
        int from_cublas = 0;
        if (syms) {
            for (int i = 0; i < n; i++) {
                if (strstr(syms[i], "libcublas") != NULL) { from_cublas = 1; break; }
            }
            free(syms);
        }
        if (from_cublas) {
            return real_dlopen(real_cuda, flags);
        }
        // Load the GCR hook driver globally so its interposed symbols win.
        void *p = real_dlopen(hook_driver, flags | RTLD_GLOBAL);
        if (p == NULL) {
            // Fallback to the real driver if the hook is unavailable.
            fprintf(stderr, "[gcr] hook driver unavailable, falling back to real cuda\n");
            CHECK_DL();
            return real_dlopen(real_cuda, flags);
        }
        return p;
    }
    return real_dlopen(filename, flags);
}
