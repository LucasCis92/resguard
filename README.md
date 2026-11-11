# resguard

Servicio para Linux que monitoriza el uso de recursos del sistema, avisa ante picos o
situaciones críticas, registra las caídas de servicios provocadas por falta de recursos
y permite ajustar dinámicamente los recursos asignados a cada servicio.

> Estado: en fase inicial de desarrollo.

## Objetivos

- **Monitorización**: CPU, memoria, disco, E/S y red, a nivel de sistema y por servicio.
- **Avisos**: alertas configurables por umbrales y por presión de recursos (PSI).
- **Registro de incidencias**: detectar y loguear caídas de servicios causadas por
  recursos (p. ej. OOM kills) o por picos, con el contexto de métricas del momento.
- **Asignación dinámica**: modificar en caliente los límites de un servicio
  (`CPUQuota`, `MemoryMax`, `IOWeight`, …) usando cgroups v2 a través de systemd.
- **CLI interactiva**: consultar el estado y aplicar cambios desde la línea de comandos.

## Arquitectura prevista

```
┌──────────────┐   socket Unix   ┌──────────────────────────────┐
│  CLI (cli)   │ ◄─────────────► │  Demonio (servicio systemd)  │
└──────────────┘                 │  - recolector de métricas    │
                                 │  - motor de alertas          │
                                 │  - vigilante de servicios    │
                                 │  - gestor de recursos        │
                                 └──────────────┬───────────────┘
                                                │ D-Bus / cgroups v2 / /proc
                                                ▼
                                           Kernel + systemd
```

Fuentes de información del sistema:

- `/proc` y `/sys/fs/cgroup` para métricas generales y por cgroup.
- `/proc/pressure/{cpu,memory,io}` (PSI) para detectar picos y contención.
- `memory.events` de cada cgroup y el journal de systemd para detectar OOM kills y caídas.
- D-Bus de systemd para consultar el estado de las unidades y cambiar sus propiedades.

## Requisitos

- Linux con **cgroups v2** (unified hierarchy) y **systemd**.
- Kernel ≥ 4.20 para PSI.
- Go ≥ 1.22 para compilar.
- Permisos de root (o los privilegios de D-Bus necesarios) para modificar recursos de servicios.

## Estructura

```
cmd/resguardd/     demonio (servicio systemd)
cmd/resguardctl/   CLI interactiva
internal/daemon/   servidor del socket y despacho de órdenes
internal/ipc/      protocolo JSON entre CLI y demonio, y cliente
internal/metrics/  lectura de métricas desde /proc
internal/version/  versión inyectada al compilar
deploy/systemd/    unidad de systemd
```

## Compilación

```bash
make build      # genera bin/resguardd y bin/resguardctl
make test vet   # pruebas y análisis estático
```

## Uso

Instalación como servicio:

```bash
sudo install -m 0755 bin/resguardd bin/resguardctl /usr/local/bin/
sudo install -m 0644 deploy/systemd/resguard.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now resguard
```

CLI:

```bash
sudo resguardctl           # modo interactivo
sudo resguardctl status    # orden única
```

Durante el desarrollo se puede ejecutar sin root usando otro socket:

```bash
./bin/resguardd -socket /tmp/resguard.sock &
./bin/resguardctl -socket /tmp/resguard.sock
```

Órdenes disponibles por ahora: `help`, `ping`, `version`, `status`.

## Licencia

Pendiente de definir.
