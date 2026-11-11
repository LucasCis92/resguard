VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/LucasCis92/resguard/internal/version.Version=$(VERSION)
# -trimpath evita que los binarios contengan rutas de la máquina donde se compilan;
# CGO_ENABLED=0 genera binarios estáticos que funcionan en cualquier distribución.
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

PREFIX  ?= /usr/local
DESTDIR ?=

# Desarrollo local: sin root, vigilando los servicios del usuario. Se usa el
# directorio privado del usuario (/run/user/UID, permisos 0700) y no /tmp, que
# es compartido y permitiría a otro usuario adelantarse a crear la ruta.
DEV_DIR    ?= $(XDG_RUNTIME_DIR)/resguard-dev
DEV_SOCKET := $(DEV_DIR)/resguard.sock
UID        := $(shell id -u)
DEV_SLICE  := user.slice/user-$(UID).slice/user@$(UID).service

.PHONY: all build test vet fmt clean install uninstall dev ctl check-go check-devdir

all: build

check-go:
	@command -v go >/dev/null || { echo "No se encuentra 'go'. Instálalo (https://go.dev/dl) o, si ya está, añádelo al PATH: export PATH=\$$PATH:/usr/local/go/bin"; exit 1; }

check-devdir:
	@[ -n "$(XDG_RUNTIME_DIR)" ] || { echo "XDG_RUNTIME_DIR no está definido; indica otro directorio privado con DEV_DIR=..."; exit 1; }

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
	rm -rf bin/

# Lanza el demonio en primer plano contra el systemd del usuario.
dev: check-devdir build
	mkdir -m 0700 -p $(DEV_DIR)
	./bin/resguardd -socket $(DEV_SOCKET) -interval 1s -systemd user \
		-slices $(DEV_SLICE) -events-file $(DEV_DIR)/events.jsonl $(ARGS)

# Abre la CLI contra el demonio de "make dev" (en otra terminal).
ctl: check-devdir
	@[ -S $(DEV_SOCKET) ] || { echo "El demonio de desarrollo no está corriendo: lánzalo antes con 'make dev' en otra terminal."; exit 1; }
	./bin/resguardctl -socket $(DEV_SOCKET) $(ARGS)

# No compila: "sudo make install" correría go como root, que normalmente no lo
# tiene en el PATH. Primero "make build" como usuario normal.
install:
	@[ -x bin/resguardd ] && [ -x bin/resguardctl ] || { echo "Faltan los binarios: ejecuta 'make build' (sin sudo) antes de 'sudo make install'."; exit 1; }
	install -Dm0755 bin/resguardd   $(DESTDIR)$(PREFIX)/bin/resguardd
	install -Dm0755 bin/resguardctl $(DESTDIR)$(PREFIX)/bin/resguardctl
	install -Dm0644 deploy/systemd/resguard.service $(DESTDIR)/etc/systemd/system/resguard.service
	install -Dm0644 deploy/sysusers.d/resguard.conf $(DESTDIR)/usr/lib/sysusers.d/resguard.conf
	@if [ -z "$(DESTDIR)" ]; then systemd-sysusers /usr/lib/sysusers.d/resguard.conf && systemctl daemon-reload; fi

uninstall:
	-systemctl disable --now resguard
	rm -f $(DESTDIR)$(PREFIX)/bin/resguardd $(DESTDIR)$(PREFIX)/bin/resguardctl
	rm -f $(DESTDIR)/etc/systemd/system/resguard.service $(DESTDIR)/usr/lib/sysusers.d/resguard.conf
	-systemctl daemon-reload
