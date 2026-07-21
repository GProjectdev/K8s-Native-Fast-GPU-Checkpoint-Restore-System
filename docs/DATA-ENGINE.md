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
`GCR_DATA_DIR`. The blob must be on a **host-visible** dir so the agent can persist it
next to the tar; the benchmark/samples use a hostPath **`/var/lib/gcr-data`**. For the
paper's speed, mount that path on **tmpfs (RAM)** on the node.

## Overlapped copy engine (freeze/remap)
freeze/remap copy GPU<->blob with a GCR-style pipeline: **pinned double-buffer + async
CUDA streams** overlap the PCIe copy with the host<->blob `memcpy` (multi-threaded), so
D2H/H2D approach PCIe bandwidth instead of the old serial ~0.7 GB/s. Falls back to a
serial pageable copy if `cudaHostAlloc`/async symbols are unavailable. Put `GCR_DATA_DIR`
on **tmpfs** so the host<->blob memcpy is RAM-speed (otherwise disk write-back caps it).

## Control+CPU path — CRIUgpu
After freeze, the GPU holds only control state; the agent calls the kubelet
ContainerCheckpoint API → CRI-O + CRIU + `cuda_plugin` checkpoint the GPU control
state and the CPU process into the tar. Because the model data is in the external
blob, the tar is smaller and the cuda_plugin GPU dump is cheap.

## What to verify on the cluster
1. **Tar shrinks**: the `gcr` tar should now be ~model-size smaller than `baseline`
   (previously they were identical). Compare `tar_bytes` in the benchmark CSV.
2. **CRIU excludes the blob**: if CRIU errors on the `data.blob` mapping or still
   dumps it, the `GCR_DATA_DIR` mount may need to be treated as external. The agent
   `munmap`s + closes the blob before the dump so CRIU can't serialize it; if needed,
   use a tmpfs hostPath or a CRIU `--external`/ext-mount mapping. Check `dump.log` and
   the tar size. (Note: `GCR_DATA_DIR` must be a **host-visible hostPath**, not an
   `emptyDir`, so the agent can copy the blob to storage.)
3. **Bandwidth**: freeze `bytes/time` should approach PCIe (pinned staging) vs the
   old ~1.2 GB/s pageable copy.

## The checkpoint is tar + blob (both needed to restore)
IMPORTANT: the `.tar` alone is **not** a complete checkpoint anymore. It holds CPU +
GPU control state; the GPU **data** lives in `data.blob`. To restore later you need
**both**.

- The interceptor writes the blob to `GCR_DATA_DIR` on a **host-visible** dir
  (`/var/lib/gcr-data`, a hostPath the agent also mounts; put it on tmpfs for RAM speed).
- After CRIUgpu, the agent copies `<GCR_DATA_DIR>/<podUID>/data.blob` next to the tar as
  `checkpoint-...blob`, so the stored checkpoint = `{...tar, ...blob}` and is complete.
  Toggle with `GCR_PERSIST_BLOB=false` (then the blob stays local = **in-place resume
  only**, faster store but not restorable elsewhere).

### Honest tradeoff
For a **durable** checkpoint the data must be written to storage regardless — so total
bytes written ≈ baseline (which put the data inside the tar). GCR's win for the durable
case is the **fast pinned copy** and the option to write the blob **asynchronously**
(off the critical path) or ship it **RAM→RAM** for live migration — not avoiding the
write. The dramatic tar/store reduction applies to **in-place resume** and to
**migration/restore latency**, which is where the paper's numbers come from.
