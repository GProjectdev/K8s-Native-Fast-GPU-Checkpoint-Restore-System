import os, time
import tensorflow as tf
for g in tf.config.list_physical_devices('GPU'):
    tf.config.experimental.set_memory_growth(g, True)   # don't grab all 40GB
import numpy as np
name = os.environ.get("MODEL", "ResNet50")
model = getattr(tf.keras.applications, name)(weights="imagenet")
x = np.random.rand(1, *model.input_shape[1:]).astype("float32")
for _ in range(3):
    model.predict(x, verbose=0)   # warmup
print("READY framework=tensorflow model=%s" % name, flush=True)
while True:
    time.sleep(3600)
