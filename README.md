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

Para ver los logs y diagnosticar problemas, consulta [Logs y diagnóstico](#logs-y-diagnóstico).

### CLI

```bash
resguardctl            # modo interactivo
resguardctl status     # orden única
resguardctl watch      # vista en vivo
```

| Orden | Qué muestra |
|-------|-------------|
| `status` | CPU, memoria, swap, carga, PSI y número de incidencias en la última hora |
| `watch [segundos]` | vista en vivo a pantalla completa: barras de uso, lo que más consume e incidencias recientes. Permite filtrar y ver los procesos de cada aplicación (ver abajo) |
| `history [n]` | las últimas *n* muestras (20 por defecto) |
| `services [cpu\|mem] [apps\|servicios\|contenedores\|sesiones] [n]` | lo que más consume, con su tipo, límite de memoria y OOM kills. Por ejemplo, `services apps mem` muestra las aplicaciones que más memoria usan |
| `events [n]` | las últimas *n* incidencias |
| `event <id>` | el detalle de una incidencia, con el estado del sistema y lo que más consumía en ese momento |
| `logs [n] [avisos] [seguir]` | los logs del servicio: las últimas *n* líneas (50 por defecto), solo avisos y errores, o en vivo (`Ctrl+C` vuelve a la CLI) |
| `config` | la configuración en uso y de qué archivo se cargó |
| `help` | todas las órdenes con su uso |

En el modo interactivo:

- **Flechas arriba/abajo**: historial de órdenes, que se conserva entre sesiones en
  `~/.local/state/resguard/history` (solo legible por tu usuario). Como en bash, las líneas que
  empiezan por espacio no se guardan.
- **Tab**: autocompleta órdenes y argumentos; si hay varias opciones, las muestra.
- **Ctrl+D**, **Ctrl+C** o `exit`: salir.
- Si el demonio se reinicia, la CLI se reconecta sola.

#### La vista `watch`

`resguardctl watch` se actualiza cada 2 segundos (o los indicados: `watch 1`). Tiene dos pantallas:

**Lista de servicios, aplicaciones y contenedores**

| Tecla | Acción |
|-------|--------|
| `↑` `↓` (o `k` `j`), `RePág` `AvPág`, `Inicio` `Fin` | elegir una fila |
| `Enter` o `→` | ver los procesos de la fila elegida |
| `a` / `s` / `d` / `t` | mostrar solo apps, servicios, contenedores (Docker) o todo |
| `c` / `m` | ordenar por CPU o por memoria |
| `q` o `Esc` | salir |

**Procesos de una aplicación o servicio**: PID, usuario, CPU, memoria, hilos y qué es cada
proceso. En Chrome y en aplicaciones Electron (como VS Code) se indica su papel: `renderer` (una
pestaña o extensión), `gpu-process`, `utility: audio`, `utility: video_capture` (la cámara),
`utility: network`… Así se ve, por ejemplo, si en una videollamada el consumo viene del video, de la
GPU o de otra pestaña. `Esc`, `←` o Retroceso vuelven a la lista; `c`/`m` ordenan; `q` sale.

Por privacidad, nunca se muestra la línea de órdenes completa de un proceso (podría contener
contraseñas o tokens): solo su nombre y, en Chromium/Electron, su papel.

Los colores se desactivan con la variable de entorno `NO_COLOR=1`; la fila elegida se marca
también con `›`.

### Incidencias que se registran

| Tipo | Cuándo |
|------|--------|
| `peak_start` / `peak_end` | un recurso supera su umbral y cuando vuelve a la normalidad, con la duración y el valor máximo del pico. Usa histéresis para no generar ruido cuando el valor oscila en el umbral |
| `oom_kill` | el contador `oom_kill` del cgroup de un servicio aumenta (incluso si el servicio sobrevive) |
| `service_failed` | systemd informa de que un servicio cayó: código de salida, señal, OOM, timeout, watchdog… y si se reiniciará |

Cada incidencia incluye un contexto con la CPU, la memoria, la PSI y lo que más CPU y memoria
consumía en ese momento. Se vigilan:

| Tipo | Ejemplos | Cómo se muestra |
|------|----------|-----------------|
| servicio | `nginx.service`, `mariadb.service` | el nombre de la unidad |
| app | Chrome, VS Code, Terminal | el nombre de la aplicación (`app-com.google.Chrome-5016.scope` → `Chrome`) |
| contenedor | contenedores Docker y Podman | el nombre del contenedor (`laravel_app`) |
| sesión | sesiones de ssh o consola | `sesión 6` |

Las aplicaciones se ven gracias a `user.slice` en `services.slices`; quítalo de la configuración si
no quieres que los miembros del grupo `resguard` vean qué aplicaciones usan los demás usuarios.

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
  slices: [system.slice, user.slice]
  ignore: ["snap.*"]
```

| Clave | Significado | Por defecto |
|-------|-------------|-------------|
| `interval` | periodo de muestreo | `2s` |
| `history` | historial en memoria para `history` y `watch` | `1h` |
| `thresholds.cpu` / `memory` | % a partir del cual hay un pico; termina al bajar 10 / 5 puntos | 90 / 90 |
| `thresholds.psi_*` | presión (PSI some avg10) de CPU, memoria y E/S; termina al bajar a la mitad | 40 / 10 / 30 |
| `thresholds.sustain` | muestras seguidas por encima del umbral para registrar el pico | 2 |
| `services.slices` | slices de cgroups donde buscar qué vigilar: `system.slice` (servicios y contenedores) y `user.slice` (aplicaciones de escritorio) | `[system.slice, user.slice]` |
| `services.ignore` | unidades que no generan incidencias; admite comodines (`snap.*`, `docker-*`, `app-*`) | `[]` |

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

### Logs y diagnóstico

Lo más habitual se puede hacer desde la CLI, sin recordar comandos de systemd:

| Para… | Desde la CLI | Con las herramientas del sistema |
|-------|--------------|----------------------------------|
| ver si el servicio funciona | `resguardctl status` | `systemctl status resguard` |
| ver los logs recientes | `resguardctl logs` | `journalctl -u resguard -n 50` |
| ver solo avisos y errores | `resguardctl logs avisos` | `journalctl -u resguard -p warning` |
| seguir los logs en vivo | `resguardctl logs seguir` | `journalctl -u resguard -f` |
| ver los logs desde una hora | — | `journalctl -u resguard --since "2026-11-17 11:00" --until "11:30"` |
| ver las incidencias | `resguardctl events` | `sudo tail /var/lib/resguard/events.jsonl` |
| entender una incidencia | `resguardctl event <id>` | — |
| ver qué consume ahora | `resguardctl services` o `resguardctl watch` | — |
| ver la configuración en uso | `resguardctl config` | `sudo resguardd -check-config` |
| validar la configuración tras editarla | — | `sudo resguardd -check-config` |
| aplicar la configuración | — | `sudo systemctl reload resguard` |
| reiniciar el servicio | — | `sudo systemctl restart resguard` |

Notas:

- Para leer el journal sin `sudo`, tu usuario debe pertenecer al grupo `adm` o `systemd-journal`
  (en Ubuntu, el primer usuario ya está en `adm`).
- `events` muestra las incidencias que el demonio tiene en memoria (las últimas 1000). El registro
  completo está en `/var/lib/resguard/events.jsonl`, una incidencia por línea en JSON, legible solo
  por root. Con `jq` se puede filtrar, por ejemplo las caídas de servicios:
  `sudo jq -c 'select(.kind == "service_failed")' /var/lib/resguard/events.jsonl`.
- Si la CLI dice "permission denied", tu sesión todavía no tiene el grupo `resguard`: cierra sesión
  y vuelve a entrar, o usa `sudo resguardctl`.

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
