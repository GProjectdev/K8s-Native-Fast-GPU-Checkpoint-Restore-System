import os, time, torch
from transformers import AutoModelForCausalLM, AutoTokenizer
m = os.environ.get("MODEL", "gpt2")
tok = AutoTokenizer.from_pretrained(m)
model = AutoModelForCausalLM.from_pretrained(m, torch_dtype=torch.float16).cuda().eval()
ids = tok("Hello, world. Tell me about GPUs.", return_tensors="pt").input_ids.cuda()
with torch.no_grad():
    for _ in range(3):
        model.generate(ids, max_new_tokens=32)   # warmup: allocate KV/activations
torch.cuda.synchronize()
print("READY framework=pytorch model=%s gpu_alloc=%.0fMiB gpu_reserved=%.0fMiB" % (
    m, torch.cuda.memory_allocated()/2**20, torch.cuda.memory_reserved()/2**20), flush=True)
while True:
    time.sleep(3600)
