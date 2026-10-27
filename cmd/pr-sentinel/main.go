// Command pr-sentinel revisa Pull Requests com IA, em modo Action ou servidor.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("uso: pr-sentinel <action|server|version>")
	}
	switch args[0] {
	case "version":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("subcomando desconhecido %q", args[0])
	}
}
