// Package config carrega a configuração do pr-sentinel a partir de variáveis
// de ambiente e do arquivo opcional .pr-sentinel.yml do repositório revisado.
package config

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Secret guarda valores sensíveis e nunca os revela em logs ou formatações.
type Secret string

const redacted = "[REDACTED]"

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Reveal devolve o valor real; use apenas ao montar headers de requisição.
func (s Secret) Reveal() string { return string(s) }

// Severity ordena os níveis de severidade dos achados.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Rank permite comparar severidades; valores desconhecidos retornam -1.
func (s Severity) Rank() int {
	switch s {
	case SeverityInfo:
		return 0
	case SeverityWarning:
		return 1
	case SeverityCritical:
		return 2
	}
	return -1
}

// OnError define o que fazer quando a revisão falha.
type OnError string

const (
	OnErrorComment OnError = "comment"
	OnErrorSilent  OnError = "silent"
	OnErrorFail    OnError = "fail"
)

// Mode indica o modo de execução, que define quais campos são obrigatórios.
type Mode string

const (
	ModeAction Mode = "action"
	ModeServer Mode = "server"
)

// Config é a configuração efetiva de uma revisão.
type Config struct {
	AnthropicKey  Secret
	GitHubToken   Secret
	WebhookSecret Secret

	Model           string
	ClaudeTimeout   time.Duration
	MaxOutputTokens int
	MaxDiffBytes    int

	Language     string
	MinSeverity  Severity
	Ignore       []string
	Instructions string
	OnError      OnError

	SkipLabel string
	SkipBots  bool

	Addr string
}

// Defaults devolve a configuração padrão.
func Defaults() Config {
	return Config{
		Model:           "claude-sonnet-5-5",
		ClaudeTimeout:   120 * time.Second,
		MaxOutputTokens: 8192, // folga para o thinking adaptativo, que consome tokens de saída
		MaxDiffBytes:    120_000,
		Language:        "pt-BR",
		MinSeverity:     SeverityInfo,
		OnError:         OnErrorComment,
		SkipLabel:       "skip-review",
		SkipBots:        true,
		Addr:            ":8080",
	}
}

// FromEnv sobrepõe os padrões com as variáveis de ambiente. getenv costuma ser
// os.Getenv, mas pode ser trocado em testes.
func FromEnv(getenv func(string) string) (Config, error) {
	c := Defaults()
	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	setStr := func(dst *string, key string) {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			*dst = v
		}
	}
	setInt := func(dst *int, key string) {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fail("%s deve ser um inteiro positivo", key)
			return
		}
		*dst = n
	}

	c.AnthropicKey = Secret(strings.TrimSpace(getenv("ANTHROPIC_API_KEY")))
	c.GitHubToken = Secret(strings.TrimSpace(getenv("GITHUB_TOKEN")))
	c.WebhookSecret = Secret(strings.TrimSpace(getenv("WEBHOOK_SECRET")))

	setStr(&c.Model, "PR_SENTINEL_MODEL")
	setStr(&c.Language, "PR_SENTINEL_LANGUAGE")
	setStr(&c.SkipLabel, "PR_SENTINEL_SKIP_LABEL")
	setStr(&c.Addr, "PR_SENTINEL_ADDR")
	setInt(&c.MaxOutputTokens, "PR_SENTINEL_MAX_OUTPUT_TOKENS")
	setInt(&c.MaxDiffBytes, "PR_SENTINEL_MAX_DIFF_BYTES")

	if v := strings.TrimSpace(getenv("PR_SENTINEL_MIN_SEVERITY")); v != "" {
		c.MinSeverity = Severity(v)
	}
	if v := strings.TrimSpace(getenv("PR_SENTINEL_ON_ERROR")); v != "" {
		c.OnError = OnError(v)
	}
	if v := strings.TrimSpace(getenv("PR_SENTINEL_SKIP_BOTS")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			fail("PR_SENTINEL_SKIP_BOTS deve ser booleano")
		} else {
			c.SkipBots = b
		}
	}
	if v := strings.TrimSpace(getenv("PR_SENTINEL_CLAUDE_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			fail("PR_SENTINEL_CLAUDE_TIMEOUT deve ser uma duração válida (ex.: 90s)")
		} else {
			c.ClaudeTimeout = d
		}
	}
	if v := getenv("PR_SENTINEL_IGNORE"); strings.TrimSpace(v) != "" {
		c.Ignore = splitList(v)
	}
	c.Instructions = strings.TrimSpace(getenv("PR_SENTINEL_INSTRUCTIONS"))

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("configuração inválida: %s", strings.Join(errs, "; "))
	}
	return c, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate confere os valores e exige os segredos necessários para o modo.
func (c Config) Validate(mode Mode) error {
	var errs []string
	if c.AnthropicKey == "" {
		errs = append(errs, "ANTHROPIC_API_KEY é obrigatório")
	}
	if c.GitHubToken == "" {
		errs = append(errs, "GITHUB_TOKEN é obrigatório")
	}
	if mode == ModeServer && c.WebhookSecret == "" {
		errs = append(errs, "WEBHOOK_SECRET é obrigatório no modo servidor")
	}
	if c.Model == "" {
		errs = append(errs, "modelo não pode ser vazio")
	}
	if c.MinSeverity.Rank() < 0 {
		errs = append(errs, fmt.Sprintf("min_severity inválido %q (use info, warning ou critical)", c.MinSeverity))
	}
	switch c.OnError {
	case OnErrorComment, OnErrorSilent, OnErrorFail:
	default:
		errs = append(errs, fmt.Sprintf("on_error inválido %q (use comment, silent ou fail)", c.OnError))
	}
	if len(errs) > 0 {
		return fmt.Errorf("configuração inválida: %s", strings.Join(errs, "; "))
	}
	return nil
}
