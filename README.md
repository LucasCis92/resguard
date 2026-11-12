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
internal/metrics/  lectura de métricas desde /proc y cgroups v2
internal/collector/ muestreo periódico e historial en memoria
internal/events/   detección de picos y OOM kills, y registro de incidencias
internal/units/    vigilancia de caídas de servicios por D-Bus de systemd
internal/version/  versión inyectada al compilar
deploy/systemd/    unidad de systemd
deploy/sysusers.d/ grupo "resguard" para usar la CLI sin root
```

## Compilación

```bash
make build      # genera bin/resguardd y bin/resguardctl
make test vet   # pruebas y análisis estático
```

## Uso

### Instalación como servicio

```bash
make build
sudo make install                     # binarios, unidad de systemd y grupo "resguard"
sudo systemctl enable --now resguard
sudo usermod -aG resguard "$USER"     # opcional: usar la CLI sin sudo (requiere volver a iniciar sesión)
```

Los logs, incluidas todas las incidencias, van al journal:

```bash
journalctl -u resguard -f
journalctl -u resguard -p warning     # solo picos, OOM kills y caídas
```

Además, las incidencias se guardan en `/var/lib/resguard/events.jsonl` (una por línea, en JSON),
y el demonio recupera las más recientes al reiniciarse.

### CLI

```bash
resguardctl            # modo interactivo
resguardctl status     # orden única
resguardctl watch      # vista en vivo
```

| Orden | Qué muestra |
|-------|-------------|
| `status` | CPU, memoria, swap, carga, PSI y número de incidencias en la última hora |
| `watch [segundos]` | vista en vivo a pantalla completa: barras de uso, servicios que más consumen e incidencias recientes. `c`/`m` ordenan por CPU o memoria, `q` sale |
| `history [n]` | las últimas *n* muestras (20 por defecto) |
| `services [cpu\|mem] [n]` | los servicios que más consumen, con su límite de memoria y sus OOM kills |
| `events [n]` | las últimas *n* incidencias |
| `event <id>` | el detalle de una incidencia, con el estado del sistema y los servicios que más consumían |
| `help` | todas las órdenes con su uso |

En el modo interactivo:

- **Flechas arriba/abajo**: historial de órdenes, que se conserva entre sesiones en
  `~/.local/state/resguard/history` (solo legible por tu usuario). Como en bash, las líneas que
  empiezan por espacio no se guardan.
- **Tab**: autocompleta órdenes y argumentos; si hay varias opciones, las muestra.
- **Ctrl+D**, **Ctrl+C** o `exit`: salir.
- Si el demonio se reinicia, la CLI se reconecta sola.

Los colores se desactivan con la variable de entorno `NO_COLOR=1`.

### Incidencias que se registran

| Tipo | Cuándo |
|------|--------|
| `peak_start` / `peak_end` | un recurso supera su umbral y cuando vuelve a la normalidad, con la duración y el valor máximo del pico. Usa histéresis para no generar ruido cuando el valor oscila en el umbral |
| `oom_kill` | el contador `oom_kill` del cgroup de un servicio aumenta (incluso si el servicio sobrevive) |
| `service_failed` | systemd informa de que un servicio cayó: código de salida, señal, OOM, timeout, watchdog… y si se reiniciará |

Cada incidencia incluye un contexto con la CPU, la memoria, la PSI y los 3 servicios que más CPU
y memoria consumían en ese momento.

### Configuración

Los umbrales, el intervalo de muestreo y los servicios vigilados se ajustan en
`/etc/resguard/config.yaml`. `make install` instala un ejemplo comentado con los valores por
defecto y nunca sobrescribe uno existente. Todas las claves son opcionales.

```yaml
interval: 2s
history: 1h
thresholds:
  cpu: 90
  memory: 90
  psi_cpu: 40
  psi_memory: 10
  psi_io: 30
  sustain: 2
services:
  slices: [system.slice]
  ignore: ["snap.*"]
```

| Clave | Significado | Por defecto |
|-------|-------------|-------------|
| `interval` | periodo de muestreo | `2s` |
| `history` | historial en memoria para `history` y `watch` | `1h` |
| `thresholds.cpu` / `memory` | % a partir del cual hay un pico; termina al bajar 10 / 5 puntos | 90 / 90 |
| `thresholds.psi_*` | presión (PSI some avg10) de CPU, memoria y E/S; termina al bajar a la mitad | 40 / 10 / 30 |
| `thresholds.sustain` | muestras seguidas por encima del umbral para registrar el pico | 2 |
| `services.slices` | slices de cgroups donde buscar servicios | `[system.slice]` |
| `services.ignore` | servicios que no generan incidencias; admite comodines | `[]` |

Un umbral a `0` desactiva esa alerta. Para aplicar cambios:

```bash
sudo resguardd -check-config          # valida y muestra la configuración resultante
sudo systemctl reload resguard        # aplica sin reiniciar: se conservan historial y picos en curso
resguardctl config                    # configuración en uso
```

Si el archivo tiene un error, el servicio sigue con la configuración anterior y lo registra en el
journal indicando la línea. Por seguridad, el archivo se rechaza si otros usuarios pueden
modificarlo.

Las opciones de `resguardd` equivalentes (`-interval`, `-cpu-threshold`, … ; ver `resguardd -h`)
tienen prioridad sobre el archivo y están pensadas para el desarrollo.

## Desarrollo y pruebas locales

No hace falta root: el demonio puede vigilar los servicios de tu usuario (`systemd --user`).

```bash
make test                 # tests unitarios y de integración
make dev                  # terminal 1: demonio en primer plano, con los logs
make dev ARGS="-config deploy/config.yaml"   # probando un archivo de configuración
make ctl                  # terminal 2: CLI interactiva
make ctl ARGS=events      # o una orden suelta
```

Opciones extra para el demonio: `make dev ARGS="-cpu-threshold 50 -sustain 1"`.

Para provocar incidencias con servicios de prueba:

```bash
# Caída con reinicio automático
systemd-run --user --unit=rg-crash -p Restart=on-failure -p RestartSec=2 sh -c 'sleep 1; exit 3'

# Caída por señal
systemd-run --user --unit=rg-segv python3 -c 'import os,signal,time; time.sleep(1); os.kill(os.getpid(), signal.SIGSEGV)'

# OOM kill: el servicio entero muere por falta de memoria
systemd-run --user --unit=rg-oom -p MemoryMax=30M -p MemorySwapMax=0 \
  python3 -c 'b=[]
while True: b.append(bytearray(5<<20))'

# OOM kill de un proceso hijo; el servicio sobrevive
systemd-run --user --unit=rg-oomchild -p MemoryMax=30M -p MemorySwapMax=0 -p OOMPolicy=continue \
  sh -c 'python3 -c "b=[]
while True: b.append(bytearray(5<<20))"; sleep 300'

# Pico de CPU en todos los núcleos durante 15 s
for i in $(seq $(nproc)); do timeout 15 sh -c 'while :; do :; done' & done

# Limpieza
systemctl --user stop rg-crash rg-oomchild; systemctl --user reset-failed
```

`stress-ng` (`apt install stress-ng`) es útil para generar presión de memoria y E/S más realista.

## Licencia

Pendiente de definir.
