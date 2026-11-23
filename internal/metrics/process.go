package metrics

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// ClockTicks es la unidad de los tiempos de CPU de /proc/<pid>/stat (USER_HZ).
// Vale 100 en todas las arquitecturas que usa Linux hoy.
const ClockTicks = 100

// MaxProcesses limita los procesos que devuelve ReadProcesses.
const MaxProcesses = 1000

// Process es un proceso dentro del cgroup de una unidad.
//
// Por privacidad no se incluye la línea de órdenes completa (puede contener
// contraseñas o tokens): solo el nombre del proceso y, en navegadores basados
// en Chromium y aplicaciones Electron, su papel (renderer, gpu-process…).
type Process struct {
	PID      int    `json:"pid"`
	Name     string `json:"name"`           // nombre del proceso (/proc/<pid>/comm)
	Role     string `json:"role,omitempty"` // p. ej. "renderer" o "utility: audio"
	User     string `json:"user"`
	Threads  int    `json:"threads"`
	CPUTicks uint64 `json:"cpu_ticks"` // tiempo de CPU acumulado (utime+stime), en ClockTicks
	RSSBytes uint64 `json:"rss_bytes"`
}

// ReadProcesses devuelve los procesos del cgroup cgroupRel (relativo a la raíz
// de cgroups) y de sus subcgroups. cgroupRel debe venir de ReadServices, nunca
// directamente de un usuario.
func (r *Reader) ReadProcesses(cgroupRel string) ([]Process, error) {
	base := r.path(CgroupRoot)
	dir := filepath.Join(base, cgroupRel)
	if rel, err := filepath.Rel(base, dir); err != nil || strings.HasPrefix(rel, "..") {
		return nil, errors.New("cgroup fuera de la jerarquía")
	}

	var pids []int
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
		if err != nil {
			return nil
		}
		for _, f := range strings.Fields(string(data)) {
			if pid, err := strconv.Atoi(f); err == nil {
				pids = append(pids, pid)
			}
		}
		if len(pids) >= MaxProcesses {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	pageSize := uint64(os.Getpagesize())
	out := make([]Process, 0, len(pids))
	for _, pid := range pids {
		if len(out) >= MaxProcesses {
			break
		}
		if p, ok := r.readProcess(pid, pageSize); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// readProcess lee un proceso; devuelve false si terminó mientras se leía.
func (r *Reader) readProcess(pid int, pageSize uint64) (Process, bool) {
	p := Process{PID: pid}
	dir := r.path(filepath.Join("proc", strconv.Itoa(pid)))

	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return p, false
	}
	// El nombre va entre paréntesis y puede contener espacios: se parsea
	// a partir del último ")".
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return p, false
	}
	fields := strings.Fields(string(stat[end+1:]))
	// Tras ")": estado(3) ppid(4) … utime(14) stime(15) … num_threads(20),
	// es decir, índices 11, 12 y 17 contando desde el estado.
	if len(fields) < 18 {
		return p, false
	}
	utime, _ := strconv.ParseUint(fields[11], 10, 64)
	stime, _ := strconv.ParseUint(fields[12], 10, 64)
	p.CPUTicks = utime + stime
	p.Threads, _ = strconv.Atoi(fields[17])

	if comm, err := os.ReadFile(filepath.Join(dir, "comm")); err == nil {
		p.Name = strings.TrimSpace(string(comm))
	}
	if statm, err := os.ReadFile(filepath.Join(dir, "statm")); err == nil {
		if f := strings.Fields(string(statm)); len(f) >= 2 {
			pages, _ := strconv.ParseUint(f[1], 10, 64)
			p.RSSBytes = pages * pageSize
		}
	}
	p.User = r.processUser(filepath.Join(dir, "status"))
	if cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		// Chromium y Electron reescriben la línea de órdenes de sus subprocesos
		// como un único texto separado por espacios, así que se divide por ambos.
		p.Role = processRole(strings.Fields(strings.ReplaceAll(string(cmdline), "\x00", " ")))
	}
	return p, true
}

// processRole extrae el papel de un proceso de Chromium o Electron a partir de
// sus argumentos, sin devolver nada más de la línea de órdenes:
//
//	--type=renderer                                          → renderer
//	--type=utility --utility-sub-type=audio.mojom.AudioService → utility: audio
func processRole(args []string) string {
	var typ, sub string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--type="); ok {
			typ = v
		} else if v, ok := strings.CutPrefix(a, "--utility-sub-type="); ok {
			sub = v
		}
	}
	if typ == "" {
		return ""
	}
	typ = safeToken(typ)
	if typ == "utility" && sub != "" {
		if i := strings.Index(sub, ".mojom"); i > 0 {
			sub = sub[:i]
		}
		return "utility: " + safeToken(sub)
	}
	return typ
}

// safeToken deja solo caracteres inocuos y limita la longitud.
func safeToken(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b.WriteRune(c)
		}
		if b.Len() >= 40 {
			break
		}
	}
	return b.String()
}

func (r *Reader) processUser(statusPath string) string {
	f, err := os.Open(statusPath)
	if err != nil {
		return "?"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				break
			}
			return r.userName(fields[0])
		}
	}
	return "?"
}

func (r *Reader) userName(uid string) string {
	if n, ok := r.userNames[uid]; ok {
		return n
	}
	if r.userNames == nil {
		r.userNames = map[string]string{}
	}
	name := uid
	if u, err := user.LookupId(uid); err == nil {
		name = u.Username
	}
	r.userNames[uid] = name
	return name
}
