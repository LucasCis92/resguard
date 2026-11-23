// resguardctl is the CLI to query and control resguardd.
//
// Without arguments it opens an interactive session; with arguments it runs a
// single command:
//
//	resguardctl            # interactive mode
//	resguardctl status     # single command
//	resguardctl watch      # live view
//	resguardctl logs       # service logs
//
// Text is shown in the user's language (RESGUARD_LANG, or the locale).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/LucasCis92/resguard/internal/i18n"
	"github.com/LucasCis92/resguard/internal/ipc"
)

func main() {
	p := i18n.New(i18n.Detect())
	socketPath := flag.String("socket", ipc.DefaultSocketPath, p.T("daemon Unix socket path"))
	flag.Parse()

	s, err := newSession(*socketPath, p)
	if err != nil {
		fmt.Fprintln(os.Stderr, (&session{socket: *socketPath, p: p}).connectError(err))
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
			fmt.Fprintln(os.Stderr, p.T("error:"), err)
			os.Exit(1)
		}
	case "logs":
		if err := logs(s, args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, p.T("error:"), err)
			os.Exit(1)
		}
	default:
		if !s.run(args, os.Stdout) {
			os.Exit(1)
		}
	}
}
