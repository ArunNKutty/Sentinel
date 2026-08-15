# applications/ — userspace + kernel artifacts loaded into every Plasma VM.
#
#   make            build everything into dist/ and pack the artifacts disk
#   make build      build only
#   make pack       pack dist/ into /opt/plasma/artifacts.img
#   make clean      remove build output
#
# Anything sitting in dist/ ends up at /opt/ne inside every VM.

DIST := dist

.PHONY: all build ko app pack clean

all: build pack

build: ko app

# Built in a debian:trixie container against the guest kernel's headers — the
# host runs a different kernel entirely, so this cannot be built natively.
ko:
	./scripts/build-ko.sh

# Static binary: no libc dependency to satisfy inside the guest.
app:
	cd neapp && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags="-s -w" -o ../$(DIST)/neapp .
	@echo "==> built $(DIST)/neapp"

pack:
	./scripts/pack.sh

clean:
	rm -f $(DIST)/gluon_sim.ko $(DIST)/neapp .artifacts.img
	rm -f gluon-sim/*.o gluon-sim/*.ko gluon-sim/*.mod gluon-sim/*.mod.c \
	      gluon-sim/.*.cmd gluon-sim/Module.symvers gluon-sim/modules.order
