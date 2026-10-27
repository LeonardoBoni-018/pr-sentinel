package main

import (
	"log/slog"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/claude"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/config"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/github"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/review"
)

// buildService liga as implementações reais (GitHub e Claude) ao núcleo.
// GITHUB_API_URL (definida pelo runner, também em GHES) e ANTHROPIC_BASE_URL
// permitem apontar para outros endpoints.
func buildService(getenv func(string) string, log *slog.Logger) func(config.Config) *review.Service {
	ua := "pr-sentinel/" + version
	return func(cfg config.Config) *review.Service {
		gh := github.New(github.Options{Token: cfg.GitHubToken, BaseURL: getenv("GITHUB_API_URL"), UserAgent: ua})
		llm := claude.New(claude.Options{
			APIKey: cfg.AnthropicKey, Model: cfg.Model, BaseURL: getenv("ANTHROPIC_BASE_URL"),
			Timeout: cfg.ClaudeTimeout, UserAgent: ua,
		})
		return review.NewService(gh, llm, cfg, log)
	}
}
