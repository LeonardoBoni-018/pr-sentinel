// Command pr-sentinel revisa Pull Requests com IA, em modo Action ou servidor.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/action"
)

var version = "dev"

// actionTimeout é o orçamento total de uma execução em modo Action.
const actionTimeout = 10 * time.Minute

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("uso: pr-sentinel <action|server|version>")
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "action":
		log := slog.New(slog.NewTextHandler(stderr, nil))
		ctx, cancel := context.WithTimeout(ctx, actionTimeout)
		defer cancel()
		return action.Run(ctx, action.Deps{
			Getenv: getenv, ReadFile: os.ReadFile, Build: buildService(getenv, log), Log: log,
		})
	default:
		return fmt.Errorf("subcomando desconhecido %q", args[0])
	}
}
