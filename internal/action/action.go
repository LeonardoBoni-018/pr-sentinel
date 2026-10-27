// Package action implementa o modo GitHub Action: lê o evento pull_request
// entregue pelo runner e executa o núcleo de revisão.
package action

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/config"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/github"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/review"
)

// Deps agrupa as dependências externas, substituíveis em testes.
type Deps struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	// Build cria o Service a partir da configuração já validada.
	Build func(config.Config) *review.Service
	Log   *slog.Logger
}

// Run lê GITHUB_EVENT_NAME/GITHUB_EVENT_PATH e revisa o PR do evento.
// Eventos que não são de PR, ou ações irrelevantes, terminam com sucesso.
func Run(ctx context.Context, d Deps) error {
	switch name := d.Getenv("GITHUB_EVENT_NAME"); name {
	case "pull_request", "pull_request_target":
	default:
		d.Log.Info("evento ignorado: o pr-sentinel só atua em pull_request", "evento", name)
		return nil
	}

	cfg, err := config.FromEnv(d.Getenv)
	if err != nil {
		return err
	}
	if err := cfg.Validate(config.ModeAction); err != nil {
		return err
	}

	path := d.Getenv("GITHUB_EVENT_PATH")
	if path == "" {
		return errors.New("GITHUB_EVENT_PATH não definido (executando fora do GitHub Actions?)")
	}
	data, err := d.ReadFile(path)
	if err != nil {
		return fmt.Errorf("lendo o evento: %w", err)
	}
	ev, err := github.ParseEventBytes(data)
	if err != nil {
		return err
	}
	if !ev.Reviewable() {
		d.Log.Info("ação do evento ignorada", "acao", ev.Action)
		return nil
	}

	out, err := d.Build(cfg).Run(ctx, ev.Ref())
	if err != nil {
		return err
	}
	d.Log.Info("concluído", "pulado", out.Skipped, "falhou", out.Failed)
	return nil
}
