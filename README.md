# resguard

**English** · [Español](README.es.md)

A Linux service that monitors resource usage, records peaks and critical situations, and logs
service failures caused by lack of resources, together with what was happening on the system at
that moment. An interactive CLI shows everything live, down to the processes of each application.

> Status: early development. The first public release is v0.1.0.

## Features

- **Monitoring**: CPU, memory, swap, load and pressure (PSI) of the whole system, and the usage of
  every service, desktop application (Chrome, VS Code…) and container (Docker, Podman).
- **Incidents**: resource peaks (with hysteresis), OOM kills and service failures (exit code,
  signal, OOM, timeout…), each with the system state and the top consumers at that moment.
- **Interactive CLI**: history, completion, a live `watch` view with filters and per-application
  processes, logs and incident details.
- **Configuration** in YAML with live reload, and a hardened systemd unit.
- **English and Spanish** user interface.
- Planned: **dynamic resource allocation**, changing a service's limits (`CPUQuota`, `MemoryMax`,
  `IOWeight`…) on the fly through systemd.

## Architecture

```
┌──────────────┐  Unix socket   ┌──────────────────────────────┐
│ resguardctl  │ ◄────────────► │  resguardd (systemd service) │
│ (CLI)        │                │  - metrics collector         │
└──────────────┘                │  - peak and OOM detector     │
                                │  - service failure watcher   │
                                └──────────────┬───────────────┘
                                               │ D-Bus / cgroups v2 / /proc
                                               ▼
                                          Kernel + systemd
```

Data sources:

- `/proc` and `/sys/fs/cgroup` for system-wide and per-cgroup metrics.
- `/proc/pressure/{cpu,memory,io}` (PSI) to detect peaks and contention.
- Each cgroup's `memory.events` to detect OOM kills.
- systemd's D-Bus API to learn when and why a service failed.

## Requirements

- Linux with **cgroups v2** (unified hierarchy) and **systemd**: Ubuntu ≥ 21.10, Debian ≥ 11,
  Fedora ≥ 31 and most current distributions.
- Kernel ≥ 4.20 for PSI.
- amd64 or arm64.
- To build from source: Go ≥ 1.22.

## Installation

**Ubuntu and Debian.** Download the `.deb` for your architecture from the
[releases page](https://github.com/LucasCis92/resguard/releases) and install it:

```bash
sudo apt install ./resguard_0.1.0-1_amd64.deb
```

**Fedora, RHEL and derivatives:**

```bash
sudo dnf install ./resguard-0.1.0-1.x86_64.rpm
```

The package installs the programs, the systemd service, the configuration in
`/etc/resguard/config.yaml` and the man pages (`man resguardctl`, `man resguardd`); it creates the
`resguard` group and leaves the service running and enabled at boot. To use the CLI without
`sudo`:

```bash
sudo usermod -aG resguard "$USER"
```

and log in again.

| To… | Ubuntu / Debian | Fedora |
|-----|-----------------|--------|
| upgrade | `sudo apt install ./resguard_<new>_amd64.deb` | `sudo dnf install ./resguard-<new>.x86_64.rpm` |
| uninstall, keeping configuration and incidents | `sudo apt remove resguard` | `sudo dnf remove resguard` |
| uninstall and delete everything | `sudo apt purge resguard` | — |

Upgrades never overwrite your `/etc/resguard/config.yaml`: if the new version changes the example
file, `apt` asks you what to do.

**Verify the package before installing it** (recommended). Every release includes an attestation
signed by GitHub proving that the package was built in this repository, from its code, with no
later changes:

```bash
gh attestation verify resguard_0.1.0-1_amd64.deb --repo LucasCis92/resguard
```

Or, at least, check its SHA-256 sum against the release's `SHA256SUMS` file:

```bash
sha256sum --check --ignore-missing SHA256SUMS
```

**From source** (requires Go):

```bash
make build
sudo make install
sudo systemctl enable --now resguard
```

`make install` uses `/usr/local`; `sudo make uninstall` reverts it. If you move from this
installation to the package, run `sudo make uninstall` first: otherwise there would be two copies
and the one in `/usr/local/bin` would take precedence.

## Usage

```bash
resguardctl            # interactive mode
resguardctl status     # single command
resguardctl watch      # live view
```

| Command | What it shows |
|---------|---------------|
| `status` | CPU, memory, swap, load, PSI and the number of incidents in the last hour |
| `watch [seconds]` | full-screen live view: usage bars, top consumers and recent incidents; lets you filter and see each application's processes (see below) |
| `history [n]` | the last *n* samples (20 by default) |
| `services [cpu\|mem] [apps\|services\|containers\|sessions] [n]` | the top consumers, with their kind, memory limit and OOM kills; for example `services apps mem` lists the applications using the most memory |
| `events [n]` | the last *n* incidents |
| `event <id>` | details of an incident, with the system state and the top consumers at that moment |
| `logs [n] [warnings] [follow]` | the service logs: the last *n* lines (50 by default), warnings and errors only, or live (`Ctrl+C` goes back to the CLI) |
| `config` | the configuration in use and the file it was loaded from |
| `help` | every command with its usage |

In interactive mode:

- **Up/down arrows**: command history, kept across sessions in `~/.local/state/resguard/history`
  (readable only by you). As in bash, lines starting with a space are not saved.
- **Tab**: completes commands and arguments; with several options, it lists them.
- **Ctrl+D**, **Ctrl+C** or `exit`: quit.
- If the daemon restarts, the CLI reconnects by itself.

### The `watch` view

`resguardctl watch` refreshes every 2 seconds (or as given: `watch 1`). It has two screens.

**Services, applications and containers**

| Key | Action |
|-----|--------|
| `↑` `↓` (or `k` `j`), `PgUp` `PgDn`, `Home` `End` | select a row |
| `Enter` or `→` | show the processes of the selected row |
| `a` / `s` / `d` / `t` | show only apps, services, containers (Docker) or everything |
| `c` / `m` | sort by CPU or by memory |
| `q` or `Esc` | quit |

**Processes of an application or service**: PID, user, CPU, memory, threads and what each process
is. For Chrome and Electron applications (such as VS Code) their role is shown: `renderer` (a tab
or an extension), `gpu-process`, `utility: audio`, `utility: video_capture` (the camera),
`utility: network`… This shows, for example, whether a video call's load comes from the video, the
GPU or another tab. `Esc`, `←` or Backspace go back to the list; `c`/`m` sort; `q` quits.

For privacy, a process's full command line is never shown (it may contain passwords or tokens):
only its name and, for Chromium/Electron, its role.

Set `NO_COLOR=1` to disable colors; the selected row is also marked with `›`.

### Language

The CLI speaks English and Spanish. It follows the system language (`LANG`, `LC_MESSAGES`,
`LANGUAGE`); any language other than Spanish uses English. To choose explicitly:

```bash
export RESGUARD_LANG=es
```

Each user sees their own language: the CLI tells the daemon which language to answer in. The
daemon's own logs (journal) and the output of `resguardd -check-config` are always in English.

## Recorded incidents

| Kind | When |
|------|------|
| `peak_start` / `peak_end` | a resource goes over its threshold, and when it is back to normal, with the peak's duration and maximum. Hysteresis avoids noise when the value hovers around the threshold |
| `oom_kill` | the `oom_kill` counter of a unit's cgroup increases (even if the unit survives) |
| `service_failed` | systemd reports that a service went down: exit code, signal, OOM, timeout, watchdog… and whether it will be restarted |

Each incident carries a context with the CPU, memory, PSI and the top CPU and memory consumers at
that moment. Monitored units:

| Kind | Examples | Shown as |
|------|----------|----------|
| service | `nginx.service`, `mariadb.service` | the unit name |
| app | Chrome, VS Code, Terminal | the application name (`app-com.google.Chrome-5016.scope` → `Chrome`) |
| container | Docker and Podman containers | the container name (`laravel_app`) |
| session | ssh or console sessions | `session-6` |

Applications are visible thanks to `user.slice` in `services.slices`; remove it from the
configuration if members of the `resguard` group should not see which applications other users
run.

## Configuration

Thresholds, the sampling interval and the monitored units are set in `/etc/resguard/config.yaml`.
The package (and `make install`) installs a commented example with the default values and never
overwrites an existing one. Every key is optional.

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

| Key | Meaning | Default |
|-----|---------|---------|
| `interval` | sampling period | `2s` |
| `history` | in-memory history for `history` and `watch` | `1h` |
| `thresholds.cpu` / `memory` | % above which there is a peak; it ends after dropping 10 / 5 points | 90 / 90 |
| `thresholds.psi_*` | CPU, memory and I/O pressure (PSI some avg10); it ends after dropping to half | 40 / 10 / 30 |
| `thresholds.sustain` | consecutive samples above the threshold needed to record a peak | 2 |
| `services.slices` | cgroup slices to monitor: `system.slice` (services and containers) and `user.slice` (desktop applications) | `[system.slice, user.slice]` |
| `services.ignore` | units that never raise incidents; wildcards allowed (`snap.*`, `docker-*`, `app-*`) | `[]` |

A threshold of `0` disables that alert. To apply changes:

```bash
sudo resguardd -check-config          # validate and print the resulting configuration
sudo systemctl reload resguard        # apply without restarting: history and ongoing peaks are kept
resguardctl config                    # configuration in use
```

If the file has an error, the service keeps the previous configuration and logs the error with
its line. For security, the file is rejected if other users can modify it.

The equivalent `resguardd` options (`-interval`, `-cpu-threshold`, …; see `resguardd -h`) take
precedence over the file and are meant for development.

## Logs and troubleshooting

The most common tasks can be done from the CLI, without remembering systemd commands:

| To… | From the CLI | With the system tools |
|-----|--------------|-----------------------|
| check that the service works | `resguardctl status` | `systemctl status resguard` |
| see recent logs | `resguardctl logs` | `journalctl -u resguard -n 50` |
| see warnings and errors only | `resguardctl logs warnings` | `journalctl -u resguard -p warning` |
| follow the logs live | `resguardctl logs follow` | `journalctl -u resguard -f` |
| see logs for a time range | — | `journalctl -u resguard --since "2026-11-17 11:00" --until "11:30"` |
| see the incidents | `resguardctl events` | `sudo tail /var/lib/resguard/events.jsonl` |
| understand an incident | `resguardctl event <id>` | — |
| see what is using resources now | `resguardctl services` or `resguardctl watch` | — |
| see the configuration in use | `resguardctl config` | `sudo resguardd -check-config` |
| validate the configuration after editing it | — | `sudo resguardd -check-config` |
| apply the configuration | — | `sudo systemctl reload resguard` |
| restart the service | — | `sudo systemctl restart resguard` |

Notes:

- To read the journal without `sudo`, your user must belong to the `adm` or `systemd-journal`
  group (on Ubuntu, the first user is already in `adm`).
- `events` shows the incidents the daemon keeps in memory (the last 1000). The full log is in
  `/var/lib/resguard/events.jsonl`, one incident per line in JSON, readable only by root. `jq` can
  filter it, for example service failures:
  `sudo jq -c 'select(.kind == "service_failed")' /var/lib/resguard/events.jsonl`.
- If the CLI says "permission denied", your session does not have the `resguard` group yet: log
  out and back in, or use `sudo resguardctl`.

## Development

```
cmd/resguardd/      daemon (systemd service)
cmd/resguardctl/    interactive CLI
internal/daemon/    socket server and command dispatch
internal/ipc/       JSON protocol between the CLI and the daemon, and the client
internal/metrics/   metrics from /proc and cgroups v2, and per-unit processes
internal/collector/ periodic sampling and in-memory history
internal/events/    peak and OOM detection, and the incident log
internal/units/     service failure watching over systemd's D-Bus
internal/config/    configuration file
internal/i18n/      translations (English source strings, Spanish catalog)
internal/version/   version injected at build time
deploy/             systemd unit, sysusers group, example configuration, man pages and packaging
```

No root is needed: the daemon can watch your user's services (`systemd --user`).

```bash
make test                 # unit and integration tests
make dev                  # terminal 1: daemon in the foreground, with its logs
make dev ARGS="-config deploy/config.yaml"   # trying a configuration file
make ctl                  # terminal 2: interactive CLI
make ctl ARGS=events      # or a single command
```

Extra daemon options: `make dev ARGS="-cpu-threshold 50 -sustain 1"`.

To trigger incidents with test services:

```bash
# Failure with automatic restart
systemd-run --user --unit=rg-crash -p Restart=on-failure -p RestartSec=2 sh -c 'sleep 1; exit 3'

# Failure by signal
systemd-run --user --unit=rg-segv python3 -c 'import os,signal,time; time.sleep(1); os.kill(os.getpid(), signal.SIGSEGV)'

# OOM kill: the whole service dies out of memory
systemd-run --user --unit=rg-oom -p MemoryMax=30M -p MemorySwapMax=0 \
  python3 -c 'b=[]
while True: b.append(bytearray(5<<20))'

# OOM kill of a child process; the service survives
systemd-run --user --unit=rg-oomchild -p MemoryMax=30M -p MemorySwapMax=0 -p OOMPolicy=continue \
  sh -c 'python3 -c "b=[]
while True: b.append(bytearray(5<<20))"; sleep 300'

# CPU peak on every core for 15 s
for i in $(seq $(nproc)); do timeout 15 sh -c 'while :; do :; done' & done

# Cleanup
systemctl --user stop rg-crash rg-oomchild; systemctl --user reset-failed
```

`stress-ng` (`apt install stress-ng`) is useful for more realistic memory and I/O pressure.

### Translations

User-facing text is written in English in the code, wrapped in `p.T("…")` (or `i18n.M("…")` for
text that is translated later), and translated in `internal/i18n/es.go`. `make test` fails if a
string has no translation or if a translation has different format verbs (`%d`, `%s`…) than its
source.

## Releasing

1. Add an entry at the top of `deploy/package/changelog.yml` for the new version.
2. Check that the packages build and pass `lintian`: `make package` (they end up in `dist/`).
3. Commit, create the tag and push it:

   ```bash
   git tag v0.1.0
   git push origin v0.1.0
   ```

4. GitHub Actions builds, packages and creates a **draft** release with the packages, the SHA-256
   sums and the attestations. Review it and publish it from the releases page.

## License

[MIT](LICENSE).
