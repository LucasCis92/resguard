VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/LucasCis92/resguard/internal/version.Version=$(VERSION)
# -trimpath keeps paths of the build machine out of the binaries;
# CGO_ENABLED=0 builds static binaries that run on any distribution.
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

PREFIX  ?= /usr/local
DESTDIR ?=

# Local development: no root, watching the user's own services. It uses the
# user's private directory (/run/user/UID, mode 0700) rather than /tmp, which is
# shared and would let another user create the path first.
DEV_DIR    ?= $(XDG_RUNTIME_DIR)/resguard-dev
DEV_SOCKET := $(DEV_DIR)/resguard.sock
UID        := $(shell id -u)
DEV_SLICE  := user.slice/user-$(UID).slice/user@$(UID).service

.PHONY: all build test vet fmt clean install uninstall dev ctl check-go check-devdir package

all: build

check-go:
	@command -v go >/dev/null || { echo "'go' not found. Install it (https://go.dev/dl) or, if it is installed, add it to PATH: export PATH=\$$PATH:/usr/local/go/bin"; exit 1; }

check-devdir:
	@[ -n "$(XDG_RUNTIME_DIR)" ] || { echo "XDG_RUNTIME_DIR is not set; give another private directory with DEV_DIR=..."; exit 1; }

build: check-go
	$(GOBUILD) -o bin/resguardd ./cmd/resguardd
	$(GOBUILD) -o bin/resguardctl ./cmd/resguardctl

test: check-go
	go test ./...

vet: check-go
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin/ build/ dist/

# Runs the daemon in the foreground against the user's systemd.
dev: check-devdir build
	mkdir -m 0700 -p $(DEV_DIR)
	./bin/resguardd -socket $(DEV_SOCKET) -interval 1s -systemd user \
		-slices $(DEV_SLICE) -events-file $(DEV_DIR)/events.jsonl $(ARGS)

# Opens the CLI against the "make dev" daemon (in another terminal).
ctl: check-devdir
	@[ -S $(DEV_SOCKET) ] || { echo "The development daemon is not running: start it first with 'make dev' in another terminal."; exit 1; }
	./bin/resguardctl -socket $(DEV_SOCKET) $(ARGS)

# It does not build: "sudo make install" would run go as root, which usually
# lacks it in PATH. Run "make build" as a normal user first.
install:
	@[ -x bin/resguardd ] && [ -x bin/resguardctl ] || { echo "Binaries missing: run 'make build' (without sudo) before 'sudo make install'."; exit 1; }
	install -Dm0755 bin/resguardd   $(DESTDIR)$(PREFIX)/bin/resguardd
	install -Dm0755 bin/resguardctl $(DESTDIR)$(PREFIX)/bin/resguardctl
	@# The repository's unit points at /usr/bin (the package path); adjust it to PREFIX.
	sed 's#^ExecStart=/usr/bin/#ExecStart=$(PREFIX)/bin/#' deploy/systemd/resguard.service > bin/resguard.service
	install -Dm0644 bin/resguard.service $(DESTDIR)/etc/systemd/system/resguard.service
	install -Dm0644 deploy/sysusers.d/resguard.conf $(DESTDIR)/usr/lib/sysusers.d/resguard.conf
	install -Dm0644 deploy/man/resguardctl.1 $(DESTDIR)$(PREFIX)/share/man/man1/resguardctl.1
	install -Dm0644 deploy/man/resguardd.8 $(DESTDIR)$(PREFIX)/share/man/man8/resguardd.8
	install -Dm0644 deploy/man/es/resguardctl.1 $(DESTDIR)$(PREFIX)/share/man/es/man1/resguardctl.1
	install -Dm0644 deploy/man/es/resguardd.8 $(DESTDIR)$(PREFIX)/share/man/es/man8/resguardd.8
	@# The configuration is only installed if missing, so the user's is never overwritten.
	@if [ -e $(DESTDIR)/etc/resguard/config.yaml ]; then \
		echo "Keeping the existing configuration in /etc/resguard/config.yaml"; \
	else \
		install -Dm0644 deploy/config.yaml $(DESTDIR)/etc/resguard/config.yaml; \
	fi
	@if [ -z "$(DESTDIR)" ]; then systemd-sysusers /usr/lib/sysusers.d/resguard.conf && systemctl daemon-reload; fi

uninstall:
	-systemctl disable --now resguard
	rm -f $(DESTDIR)$(PREFIX)/bin/resguardd $(DESTDIR)$(PREFIX)/bin/resguardctl
	rm -f $(DESTDIR)$(PREFIX)/share/man/man1/resguardctl.1 $(DESTDIR)$(PREFIX)/share/man/man8/resguardd.8
	rm -f $(DESTDIR)$(PREFIX)/share/man/es/man1/resguardctl.1 $(DESTDIR)$(PREFIX)/share/man/es/man8/resguardd.8
	rm -f $(DESTDIR)/etc/systemd/system/resguard.service $(DESTDIR)/usr/lib/sysusers.d/resguard.conf
	@echo "Kept /etc/resguard (configuration) and /var/lib/resguard (incidents); delete them by hand if you no longer want them."
	-systemctl daemon-reload

# Builds the .deb and .rpm packages and the .tar.gz archives into dist/
# without publishing anything (needs goreleaser). Published releases are built
# by GitHub Actions.
package: check-go
	@command -v goreleaser >/dev/null || { echo "goreleaser is missing: go install github.com/goreleaser/goreleaser/v2@v2.20.0"; exit 1; }
	goreleaser release --snapshot --clean
