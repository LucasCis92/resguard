// resguardctl es la CLI para consultar y controlar resguardd.
//
// Sin argumentos abre una sesión interactiva; con argumentos ejecuta una sola orden:
//
//	resguardctl            # modo interactivo
//	resguardctl status     # orden única
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/LucasCis92/resguard/internal/ipc"
)

func main() {
	socketPath := flag.String("socket", ipc.DefaultSocketPath, "ruta del socket Unix del demonio")
	flag.Parse()

	client, err := ipc.Dial(*socketPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer client.Close()

	if flag.NArg() > 0 {
		if !run(client, flag.Args()) {
			os.Exit(1)
		}
		return
	}
	repl(client)
}

// run ejecuta una orden e imprime el resultado. Devuelve false si falló.
func run(client *ipc.Client, fields []string) bool {
	resp, err := client.Call(fields[0], fields[1:]...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return false
	}
	if !resp.OK {
		fmt.Fprintln(os.Stderr, "error:", resp.Error)
		return false
	}
	if resp.Output != "" {
		fmt.Println(resp.Output)
	}
	return true
}

func repl(client *ipc.Client) {
	fmt.Println(`resguardctl: escribe "help" para ver las órdenes, "exit" para salir.`)
	in := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("resguardctl> ")
		if !in.Scan() {
			fmt.Println()
			return
		}
		fields := strings.Fields(in.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "exit", "quit":
			return
		default:
			run(client, fields)
		}
	}
}
