// Package review é o núcleo do pr-sentinel: monta o prompt, valida a resposta
// da IA e publica a revisão no PR. É usado igualmente pelos modos Action e
// servidor.
package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/config"
)

// Category classifica um achado.
type Category string

const (
	CategoryBug         Category = "bug"
	CategorySecurity    Category = "security"
	CategoryPerformance Category = "performance"
	CategoryStyle       Category = "style"
	CategoryTests       Category = "tests"
)

var validCategories = map[Category]bool{
	CategoryBug: true, CategorySecurity: true, CategoryPerformance: true, CategoryStyle: true, CategoryTests: true,
}

// Finding é um achado da revisão.
type Finding struct {
	File       string          `json:"file"`
	Line       int             `json:"line"`
	Severity   config.Severity `json:"severity"`
	Category   Category        `json:"category"`
	Message    string          `json:"message"`
	Suggestion string          `json:"suggestion"`
}

// Result é a revisão estruturada devolvida pela IA.
type Result struct {
	Summary      string    `json:"summary"`
	RiskLevel    string    `json:"risk_level"`
	Findings     []Finding `json:"findings"`
	MissingTests []string  `json:"missing_tests"`
}

// Schema é o JSON Schema enviado em output_config.format. Segue as limitações
// documentadas: additionalProperties=false em todo objeto e sem
// minimum/maximum (a faixa de "line" é validada em Go).
var Schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string"},
    "risk_level": {"type": "string", "enum": ["low", "medium", "high"]},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "file": {"type": "string"},
          "line": {"type": "integer"},
          "severity": {"type": "string", "enum": ["info", "warning", "critical"]},
          "category": {"type": "string", "enum": ["bug", "security", "performance", "style", "tests"]},
          "message": {"type": "string"},
          "suggestion": {"type": "string"}
        },
        "required": ["file", "line", "severity", "category", "message", "suggestion"],
        "additionalProperties": false
      }
    },
    "missing_tests": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["summary", "risk_level", "findings", "missing_tests"],
  "additionalProperties": false
}`)

// Limites defensivos sobre o que vai parar em comentários públicos.
const (
	maxSummary      = 4000
	maxMessage      = 1500
	maxSuggestion   = 1500
	maxPath         = 300
	maxFindings     = 50
	maxMissingTests = 15
	maxMissingTest  = 500
)

// ErrInvalidResult indica resposta da IA fora do contrato.
var ErrInvalidResult = errors.New("resposta da IA fora do formato esperado")

// Parse valida o texto devolvido pela IA. Erros estruturais (JSON inválido,
// summary vazio, risk_level desconhecido) falham; achados individuais inválidos
// são descartados e contados em dropped.
func Parse(text string) (res Result, dropped int, err error) {
	text = stripFences(strings.TrimSpace(text))
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		return Result{}, 0, fmt.Errorf("%w: JSON inválido: %v", ErrInvalidResult, err)
	}

	res.Summary = clip(strings.TrimSpace(res.Summary), maxSummary)
	if res.Summary == "" {
		return Result{}, 0, fmt.Errorf("%w: summary vazio", ErrInvalidResult)
	}
	res.RiskLevel = strings.ToLower(strings.TrimSpace(res.RiskLevel))
	switch res.RiskLevel {
	case "low", "medium", "high":
	default:
		return Result{}, 0, fmt.Errorf("%w: risk_level %q", ErrInvalidResult, res.RiskLevel)
	}

	valid := make([]Finding, 0, len(res.Findings))
	for _, f := range res.Findings {
		f.File = clip(strings.TrimSpace(f.File), maxPath)
		f.Message = clip(strings.TrimSpace(f.Message), maxMessage)
		f.Suggestion = clip(strings.TrimSpace(f.Suggestion), maxSuggestion)
		f.Severity = config.Severity(strings.ToLower(string(f.Severity)))
		f.Category = Category(strings.ToLower(string(f.Category)))
		if f.File == "" || f.Message == "" || f.Line < 0 || f.Severity.Rank() < 0 || !validCategories[f.Category] || len(valid) >= maxFindings {
			dropped++
			continue
		}
		valid = append(valid, f)
	}
	res.Findings = valid

	var tests []string
	for _, t := range res.MissingTests {
		if t = clip(strings.TrimSpace(t), maxMissingTest); t != "" && len(tests) < maxMissingTests {
			tests = append(tests, t)
		}
	}
	res.MissingTests = tests
	return res, dropped, nil
}

// stripFences remove cercas ```json ... ``` caso o modelo as inclua.
func stripFences(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}

func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}
