package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		check   func(*testing.T, Config)
		wantErr string
	}{
		{
			name: "padrões",
			env:  nil,
			check: func(t *testing.T, c Config) {
				d := Defaults()
				if c.Model != "claude-sonnet-5-5" || c.Language != "pt-BR" || c.OnError != OnErrorComment || !c.SkipBots {
					t.Fatalf("padrões inesperados: %+v", c)
				}
				if c.MaxDiffBytes != d.MaxDiffBytes {
					t.Fatalf("MaxDiffBytes = %d", c.MaxDiffBytes)
				}
			},
		},
		{
			name: "sobrescreve tudo",
			env: map[string]string{
				"ANTHROPIC_API_KEY":             " k ",
				"PR_SENTINEL_MODEL":             "outro-modelo",
				"PR_SENTINEL_LANGUAGE":          "en",
				"PR_SENTINEL_MAX_DIFF_BYTES":    "1000",
				"PR_SENTINEL_MAX_OUTPUT_TOKENS": "512",
				"PR_SENTINEL_SKIP_BOTS":         "false",
				"PR_SENTINEL_CLAUDE_TIMEOUT":    "30s",
				"PR_SENTINEL_IGNORE":            "a/**, b.txt ,",
				"PR_SENTINEL_MIN_SEVERITY":      "warning",
				"PR_SENTINEL_ON_ERROR":          "silent",
			},
			check: func(t *testing.T, c Config) {
				if c.AnthropicKey.Reveal() != "k" || c.Model != "outro-modelo" || c.Language != "en" {
					t.Fatalf("config: %+v", c)
				}
				if c.MaxDiffBytes != 1000 || c.MaxOutputTokens != 512 || c.SkipBots || c.ClaudeTimeout != 30*time.Second {
					t.Fatalf("config: %+v", c)
				}
				if len(c.Ignore) != 2 || c.Ignore[1] != "b.txt" {
					t.Fatalf("ignore = %v", c.Ignore)
				}
				if c.MinSeverity != SeverityWarning || c.OnError != OnErrorSilent {
					t.Fatalf("config: %+v", c)
				}
			},
		},
		{name: "inteiro inválido", env: map[string]string{"PR_SENTINEL_MAX_DIFF_BYTES": "abc"}, wantErr: "PR_SENTINEL_MAX_DIFF_BYTES"},
		{name: "inteiro negativo", env: map[string]string{"PR_SENTINEL_MAX_OUTPUT_TOKENS": "-1"}, wantErr: "PR_SENTINEL_MAX_OUTPUT_TOKENS"},
		{name: "bool inválido", env: map[string]string{"PR_SENTINEL_SKIP_BOTS": "talvez"}, wantErr: "SKIP_BOTS"},
		{name: "duração inválida", env: map[string]string{"PR_SENTINEL_CLAUDE_TIMEOUT": "x"}, wantErr: "TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := FromEnv(env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, quer conter %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, c)
		})
	}
}

func TestValidate(t *testing.T) {
	valid := func() Config {
		c := Defaults()
		c.AnthropicKey, c.GitHubToken, c.WebhookSecret = "a", "g", "w"
		return c
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		mode    Mode
		wantErr string
	}{
		{"ok action", func(*Config) {}, ModeAction, ""},
		{"ok server", func(*Config) {}, ModeServer, ""},
		{"sem chave", func(c *Config) { c.AnthropicKey = "" }, ModeAction, "ANTHROPIC_API_KEY"},
		{"sem token", func(c *Config) { c.GitHubToken = "" }, ModeAction, "GITHUB_TOKEN"},
		{"sem webhook secret no action", func(c *Config) { c.WebhookSecret = "" }, ModeAction, ""},
		{"sem webhook secret no server", func(c *Config) { c.WebhookSecret = "" }, ModeServer, "WEBHOOK_SECRET"},
		{"severidade inválida", func(c *Config) { c.MinSeverity = "x" }, ModeAction, "min_severity"},
		{"on_error inválido", func(c *Config) { c.OnError = "x" }, ModeAction, "on_error"},
		{"modelo vazio", func(c *Config) { c.Model = "" }, ModeAction, "modelo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c)
			err := c.Validate(tt.mode)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, quer conter %q", err, tt.wantErr)
			}
		})
	}
}

func TestSecretNeverLeaks(t *testing.T) {
	const raw = "sk-super-secreto"
	c := Config{AnthropicKey: Secret(raw), GitHubToken: Secret(raw)}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", "key", c.AnthropicKey, "cfg", c)
	outs := []string{
		fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c),
		fmt.Sprintf("%s", c.AnthropicKey), buf.String(), //nolint:gosimple
	}
	for _, o := range outs {
		if strings.Contains(o, raw) {
			t.Fatalf("segredo vazou em %q", o)
		}
	}
	if c.AnthropicKey.Reveal() != raw {
		t.Fatal("Reveal deve devolver o valor real")
	}
}

func TestSeverityRank(t *testing.T) {
	if !(SeverityInfo.Rank() < SeverityWarning.Rank() && SeverityWarning.Rank() < SeverityCritical.Rank()) {
		t.Fatal("ordem de severidade incorreta")
	}
	if Severity("x").Rank() != -1 {
		t.Fatal("severidade desconhecida deve ser -1")
	}
}
