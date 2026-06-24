# Builds the LD_PRELOAD interception shim.
#
#   make            -> libgcr-interceptor.so
#   make install DESTDIR=/var/lib/gpu-cr/lib
#
# NOTE: The GCR *hook driver* (libcuda.so) that performs the actual selective
# cuMem* interception must be built from the upstream repo
# (https://github.com/thustorage/GCR, GCR/build.sh) and placed next to this
# shim. This Makefile only builds the dlopen shim.

CC      ?= gcc
CFLAGS  ?= -O2 -fPIC -Wall -Wextra
LDFLAGS ?= -shared -ldl
TARGET   = libgcr-interceptor.so
DESTDIR ?= /var/lib/gpu-cr/lib

.PHONY: all clean install

all: $(TARGET)

$(TARGET): preload.c
	$(CC) $(CFLAGS) preload.c -o $(TARGET) $(LDFLAGS)

install: $(TARGET)
	install -d $(DESTDIR)
	install -m 0755 $(TARGET) $(DESTDIR)/$(TARGET)

clean:
	rm -f $(TARGET)
