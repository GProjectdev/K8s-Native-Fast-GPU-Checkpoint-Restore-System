# Builds the LD_PRELOAD selective CUDA-interception library.
#
#   make            -> libgcr-interceptor.so
#   make install DESTDIR=/var/lib/gpu-cr/lib
#
# This is a self-contained interceptor (no CUDA toolkit/headers required): it
# interposes the CUDA memory APIs and runs a control-channel watcher that ACKs
# the Node Agent's checkpoint request and dumps the intercepted buffer info.

CC      ?= gcc
CFLAGS  ?= -O2 -fPIC -Wall -Wextra
LDFLAGS ?= -shared -ldl -lpthread
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
