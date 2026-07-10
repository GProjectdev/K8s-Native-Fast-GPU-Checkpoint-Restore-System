# Selective Interception data engine (GCR-aligned)

Checkpoint splits into **data** (interception) and **control+CPU** (CRIUgpu), following
the GCR paper (thustorage/GCR).

## Data path — in-Pod interceptor (`interceptor/preload.c`)
- `cudaMalloc` is served by our VMM allocator (`cuMemAddressReserve`+`cuMemCreate`+
  `cuMemMap`+`cuMemSetAccess`), 2MB-aligned, tracked in `g_owned[]`.
- **freeze** (on the agent's checkpoint signal): each owned buffer is copied D2H
  through a **pinned staging buffer** (`cudaHostAlloc`, 128MB chunks) into an
  **external MAP_SHARED blob file** at `$GCR_DATA_DIR/<podUID>/data.blob`, then the
  physical GPU memory is released (`cuMemUnmap`+`cuMemRelease`) while the VA is kept.
- The blob is an external file mapping, so **CRIU does NOT serialize it into the
  checkpoint tar** (unlike the previous in-process `malloc`, which CRIU dumped).
- **remap** (on resume): recreate physical + map the same VA + copy H2D from the blob.

This mirrors GCR: their offload target is `/mnt/huge` hugepage shared memory; ours is
`GCR_DATA_DIR`. Put `GCR_DATA_DIR` on **tmpfs (RAM)** for the paper's speed — the
benchmark mounts it as an `emptyDir{medium: Memory}`.

## Control+CPU path — CRIUgpu
After freeze, the GPU holds only control state; the agent calls the kubelet
ContainerCheckpoint API → CRI-O + CRIU + `cuda_plugin` checkpoint the GPU control
state and the CPU process into the tar. Because the model data is in the external
blob, the tar is smaller and the cuda_plugin GPU dump is cheap.

## What to verify on the cluster
1. **Tar shrinks**: the `gcr` tar should now be ~model-size smaller than `baseline`
   (previously they were identical). Compare `tar_bytes` in the benchmark CSV.
2. **CRIU excludes the blob**: if CRIU errors on the `data.blob` mapping or still
   dumps it, the `GCR_DATA_DIR` mount may need to be treated as external. Options:
   `emptyDir{medium:Memory}` (default in the benchmark), a tmpfs hostPath, or a CRIU
   `--external`/ext-mount mapping. Check `dump.log` and the tar size.
3. **Bandwidth**: freeze `bytes/time` should approach PCIe (pinned staging) vs the
   old ~1.2 GB/s pageable copy.

## Persistence note
The blob is the data half of the checkpoint. For **in-place resume** (freeze→checkpoint
→keep running) it stays in RAM and `remap` reads it — nothing extra needed. For
**migration / restore-from-scratch**, the blob must be shipped alongside the tar
(async copy out of RAM); that step is intentionally kept off the checkpoint critical
path.
