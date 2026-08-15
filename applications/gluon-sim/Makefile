# Kbuild makefile for gluon_sim. Invoked from inside the debian:trixie build
# container by ../scripts/build-ko.sh, against the guest kernel's headers —
# never against the host's, which is a different kernel entirely.
obj-m += gluon_sim.o

KDIR ?= /lib/modules/$(shell uname -r)/build
PWD  := $(shell pwd)

.PHONY: all clean

all:
	$(MAKE) -C $(KDIR) M=$(PWD) modules

clean:
	$(MAKE) -C $(KDIR) M=$(PWD) clean
