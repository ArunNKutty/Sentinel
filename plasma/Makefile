PREFIX      ?= /usr/local
BINDIR      := $(PREFIX)/bin
SOCKET_GROUP := plasma
# Under sudo, add the invoking user to the group rather than root.
INSTALL_USER := $(if $(SUDO_USER),$(SUDO_USER),$(USER))
INSTALL_HOME := $(shell getent passwd $(INSTALL_USER) | cut -d: -f6)
# Wireshark's per-user extcap dir. Preferred over the system one
# (/usr/lib/*/wireshark/extcap): no arch-specific path to guess, and it does
# not mix our binary in with the distro's. Override for a system-wide install.
EXTCAP_DIR  ?= $(INSTALL_HOME)/.config/wireshark/extcap

.PHONY: build test install uninstall install-service uninstall-service \
        install-extcap uninstall-extcap ui-dev

build:
	go build -o bin/plasma        ./cmd/plasma
	go build -o bin/plasmad       ./cmd/plasmad
	go build -o bin/plasma-extcap ./cmd/plasma-extcap

test:
	go test ./...

# Installs both binaries and makes the control socket usable without sudo:
# plasmad chgrps the socket to $(SOCKET_GROUP) at startup, so members of that
# group can drive the daemon. The daemon itself still runs as root.
install: build
	install -d $(BINDIR)
	install -m 0755 bin/plasma  $(BINDIR)/plasma
	install -m 0755 bin/plasmad $(BINDIR)/plasmad
	getent group $(SOCKET_GROUP) >/dev/null || groupadd --system $(SOCKET_GROUP)
	id -nG $(INSTALL_USER) | tr ' ' '\n' | grep -qx $(SOCKET_GROUP) || \
		usermod -aG $(SOCKET_GROUP) $(INSTALL_USER)
	@echo
	@echo "Installed $(BINDIR)/plasma and $(BINDIR)/plasmad."
	@echo "$(INSTALL_USER) is in the '$(SOCKET_GROUP)' group — log out and back in"
	@echo "(or run 'newgrp $(SOCKET_GROUP)') for it to apply to this session."

uninstall:
	rm -f $(BINDIR)/plasma $(BINDIR)/plasmad $(BINDIR)/plasma-extcap
	@echo "Left the '$(SOCKET_GROUP)' group in place; remove it with 'groupdel $(SOCKET_GROUP)'."

# Separate from `install` so installing plasma never silently edits a user's
# Wireshark configuration.
install-extcap: build
	install -d $(EXTCAP_DIR)
	install -m 0755 bin/plasma-extcap $(EXTCAP_DIR)/plasma-extcap
	@if [ -n "$(SUDO_USER)" ]; then chown -R $(INSTALL_USER) $(EXTCAP_DIR); fi
	@echo
	@echo "Installed the Wireshark plugin to $(EXTCAP_DIR)."
	@echo "Plasma VMs now appear in Wireshark's interface list (restart Wireshark first)."
	@echo "The plugin runs as you, so you must be in the '$(SOCKET_GROUP)' group."

uninstall-extcap:
	rm -f $(EXTCAP_DIR)/plasma-extcap

# The UI talks to plasmad over its unix socket, so it has to run as a member of
# the plasma group — hence sg rather than a bare npm run.
ui-dev:
	cd ../plasma-ui && sg $(SOCKET_GROUP) -c "npm run dev"

install-service: install
	install -m 0644 packaging/plasmad.service /etc/systemd/system/plasmad.service
	systemctl daemon-reload
	systemctl enable --now plasmad
	@echo "plasmad is running under systemd: systemctl status plasmad"

uninstall-service:
	-systemctl disable --now plasmad
	rm -f /etc/systemd/system/plasmad.service
	systemctl daemon-reload
