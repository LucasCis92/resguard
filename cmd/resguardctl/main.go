// resguardctl es la CLI para consultar y controlar resguardd.
//
// Sin argumentos abre una sesión interactiva; con argumentos ejecuta una sola orden:
//
//	resguardctl            # modo interactivo
//	resguardctl status     # orden única
//	resguardctl watch      # vista en vivo
//	resguardctl logs       # logs del servicio
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/LucasCis92/resguard/internal/ipc"
)

func main() {
	socketPath := flag.String("socket", ipc.DefaultSocketPath, "ruta del socket Unix del demonio")
	flag.Parse()

	s, err := newSession(*socketPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, fs.ErrPermission) {
			fmt.Fprintln(os.Stderr, "Sugerencia: usa sudo, o añade tu usuario al grupo resguard (sudo usermod -aG resguard $USER) y vuelve a iniciar sesión.")
		}
		os.Exit(1)
	}
	defer s.close()

	if flag.NArg() == 0 {
		repl(s)
		return
	}

	args := flag.Args()
	switch args[0] {
	case "watch":
		if err := watch(s, args[1:], false); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "logs":
		if err := logs(s, args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		if !s.run(args, os.Stdout) {
			os.Exit(1)
		}
	}
}
