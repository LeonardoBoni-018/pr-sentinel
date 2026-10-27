package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/claude"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/config"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/diff"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/github"
)

// maxInline limita os comentários inline por execução, para não inundar o PR.
const maxInline = 20

// Service orquestra a revisão de um PR. Depende apenas de interfaces.
type Service struct {
	GH    github.API
	LLM   claude.Completer
	Cfg   config.Config
	Log   *slog.Logger
	nonce func() string
}

// NewService cria um Service com logger padrão se log for nil.
func NewService(gh github.API, llm claude.Completer, cfg config.Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{GH: gh, LLM: llm, Cfg: cfg, Log: log, nonce: newNonce}
}

// Outcome resume o que aconteceu em uma execução.
type Outcome struct {
	Skipped   string // motivo, quando o PR foi pulado
	Failed    string // descrição curta, quando a revisão falhou mas não bloqueou
	Risk      string
	Findings  int
	Inline    int
	Truncated bool
	Usage     claude.Usage
}

// Run revisa o PR. Retorna erro apenas se a execução deve ser considerada
// falha (on_error=fail ou impossibilidade de sequer consultar o PR).
func (s *Service) Run(ctx context.Context, ref github.PullRef) (Outcome, error) {
	log := s.Log.With("pr", ref.String())

	pr, err := s.GH.GetPullRequest(ctx, ref)
	if err != nil {
		return Outcome{}, fmt.Errorf("consultando PR: %w", err)
	}
	if reason := s.skipReason(pr); reason != "" {
		log.Info("PR ignorado", "motivo", reason)
		return Outcome{Skipped: reason}, nil
	}

	cfg, cfgWarn := s.repoConfig(ctx, ref, pr, log)

	out, err := s.review(ctx, ref, pr, cfg, cfgWarn, log)
	if err != nil {
		return s.handleFailure(ctx, ref, cfg, out, err, log)
	}
	log.Info("revisão publicada", "risco", out.Risk, "achados", out.Findings, "inline", out.Inline,
		"truncado", out.Truncated, "tokens_in", out.Usage.InputTokens, "tokens_out", out.Usage.OutputTokens)
	return out, nil
}

func (s *Service) skipReason(pr github.PullRequest) string {
	switch {
	case pr.State != "" && pr.State != "open":
		return "PR não está aberto"
	case s.Cfg.SkipBots && strings.EqualFold(pr.AuthorType, "Bot"):
		return "PR criado por bot"
	}
	if s.Cfg.SkipLabel != "" {
		for _, l := range pr.Labels {
			if strings.EqualFold(l, s.Cfg.SkipLabel) {
				return fmt.Sprintf("label %q presente", s.Cfg.SkipLabel)
			}
		}
	}
	return ""
}

// repoConfig aplica o .pr-sentinel.yml lido da branch BASE (um PR não pode
// alterar as regras que o revisam). Qualquer problema vira aviso, nunca falha.
func (s *Service) repoConfig(ctx context.Context, ref github.PullRef, pr github.PullRequest, log *slog.Logger) (config.Config, string) {
	data, found, err := s.GH.GetFile(ctx, ref.Repo, config.RepoFileName, pr.BaseSHA)
	if err != nil {
		log.Warn("não foi possível ler a configuração do repositório", "erro", err)
		return s.Cfg, ""
	}
	if !found {
		return s.Cfg, ""
	}
	rf, err := config.ParseRepoFile(strings.NewReader(string(data)))
	if err != nil {
		log.Warn("configuração do repositório inválida", "erro", err)
		return s.Cfg, err.Error()
	}
	merged := s.Cfg.Apply(rf)
	if err := merged.Validate(config.ModeAction); err != nil {
		log.Warn("configuração do repositório rejeitada", "erro", err)
		return s.Cfg, err.Error()
	}
	return merged, ""
}

func (s *Service) review(ctx context.Context, ref github.PullRef, pr github.PullRequest, cfg config.Config, cfgWarn string, log *slog.Logger) (Outcome, error) {
	var out Outcome

	files, err := s.GH.ListFiles(ctx, ref)
	if err != nil {
		return out, fmt.Errorf("listando arquivos: %w", err)
	}
	kept, skipped := diff.Filter(files, cfg.Ignore)
	if len(kept) == 0 {
		out.Skipped = "nenhum arquivo revisável"
		log.Info("PR ignorado", "motivo", out.Skipped, "arquivos_ignorados", len(skipped))
		return out, nil
	}
	trunc := diff.Truncate(kept, cfg.MaxDiffBytes)
	out.Truncated = trunc.Truncated()
	if len(trunc.Files) == 0 {
		return out, errors.New("limite de tamanho do diff pequeno demais para qualquer arquivo")
	}

	prompt := BuildPrompt(cfg, pr, diff.Format(trunc.Files), trunc.Omitted, s.nonce())
	resp, err := s.LLM.Complete(ctx, claude.Request{
		System: prompt.System, User: prompt.User, MaxTokens: cfg.MaxOutputTokens, Schema: Schema,
	})
	out.Usage = resp.Usage
	if err != nil {
		return out, fmt.Errorf("chamando a IA: %w", err)
	}
	result, dropped, err := Parse(resp.Text)
	if err != nil {
		return out, err
	}
	if dropped > 0 {
		log.Warn("achados inválidos descartados", "quantidade", dropped)
	}
	out.Risk = result.RiskLevel

	findings := filterAndSort(result.Findings, cfg.MinSeverity)
	out.Findings = len(findings)

	existing := s.existingInlineIDs(ctx, ref, log)
	inline, others := splitInline(findings, trunc.Files, existing)
	if len(inline) > 0 {
		if err := s.postInline(ctx, ref, pr, cfg, inline, existing); err != nil {
			// Comentário inline é "best effort": os achados vão para o resumo.
			log.Warn("falha ao publicar comentários inline; movendo para o resumo", "erro", err)
			others = append(inline, others...)
			sortFindings(others)
			inline = nil
		}
	}
	out.Inline = len(inline)

	body := RenderSummary(SummaryInput{
		Result: result, Others: others, Total: len(findings), Inline: len(inline),
		Omitted: trunc.Omitted, Partial: trunc.Partial, IgnoredFiles: len(skipped),
		ConfigWarning: cfgWarn, Language: cfg.Language, Model: cfg.Model, HeadSHA: pr.HeadSHA,
	})
	if err := s.upsertSummary(ctx, ref, body, log); err != nil {
		return out, fmt.Errorf("publicando resumo: %w", err)
	}
	return out, nil
}

func filterAndSort(in []Finding, min config.Severity) []Finding {
	out := make([]Finding, 0, len(in))
	for _, f := range in {
		if f.Severity.Rank() >= min.Rank() {
			out = append(out, f)
		}
	}
	sortFindings(out)
	return out
}

func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Severity.Rank() != b.Severity.Rank() {
			return a.Severity.Rank() > b.Severity.Rank()
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
}

// splitInline separa o que pode virar comentário inline (linha existente no
// diff enviado e ainda não comentada em pushes anteriores) do restante.
// Achados já publicados inline antes são omitidos do resumo também, pois o
// comentário continua no PR.
func splitInline(findings []Finding, sent []diff.File, existing map[string]bool) (inline, others []Finding) {
	valid := make(map[string]map[int]bool, len(sent))
	for _, f := range sent {
		valid[f.Path] = diff.RightLines(f.Patch)
	}

	for _, f := range findings {
		switch {
		case existing[InlineID(f)]:
			inline = append(inline, f) // já está no PR; contado, mas não republicado
		case f.Line > 0 && valid[f.File][f.Line] && countNew(inline, existing) < maxInline:
			inline = append(inline, f)
		default:
			others = append(others, f)
		}
	}
	return inline, others
}

func countNew(fs []Finding, existing map[string]bool) int {
	n := 0
	for _, f := range fs {
		if !existing[InlineID(f)] {
			n++
		}
	}
	return n
}

// existingInlineIDs devolve os IDs (marcador oculto) dos achados já comentados
// inline em execuções anteriores. Falha ao listar vira aviso: no pior caso
// há comentários repetidos.
func (s *Service) existingInlineIDs(ctx context.Context, ref github.PullRef, log *slog.Logger) map[string]bool {
	ids := map[string]bool{}
	rcs, err := s.GH.ListReviewComments(ctx, ref)
	if err != nil {
		log.Warn("não foi possível listar comentários inline existentes", "erro", err)
		return ids
	}
	for _, rc := range rcs {
		i := strings.Index(rc.Body, inlinePrefix)
		if i < 0 {
			continue
		}
		rest := rc.Body[i+len(inlinePrefix):]
		if j := strings.Index(rest, " -->"); j > 0 {
			ids[rest[:j]] = true
		}
	}
	return ids
}

// postInline publica apenas os achados ainda não comentados.
func (s *Service) postInline(ctx context.Context, ref github.PullRef, pr github.PullRequest, cfg config.Config, inline []Finding, existing map[string]bool) error {
	var comments []github.InlineComment
	for _, f := range inline {
		if existing[InlineID(f)] {
			continue
		}
		comments = append(comments, github.InlineComment{Path: f.File, Line: f.Line, Body: RenderInline(f, cfg.Language)})
	}
	if len(comments) == 0 {
		return nil
	}
	return s.GH.CreateReview(ctx, ref, github.ReviewInput{
		CommitID: pr.HeadSHA,
		Body:     RenderReviewBody(cfg.Language, len(comments)),
		Comments: comments,
	})
}

// upsertSummary atualiza o comentário marcado ou cria um novo.
func (s *Service) upsertSummary(ctx context.Context, ref github.PullRef, body string, log *slog.Logger) error {
	comments, err := s.GH.ListComments(ctx, ref)
	if err != nil {
		return err
	}
	for _, c := range comments {
		if strings.Contains(c.Body, Marker) {
			err := s.GH.UpdateComment(ctx, ref.Repo, c.ID, body)
			if err == nil {
				return nil
			}
			// Pode ser um comentário alheio com o marcador forjado: cria um novo.
			log.Warn("não foi possível atualizar o comentário anterior; criando outro", "erro", err)
			break
		}
	}
	return s.GH.CreateComment(ctx, ref, body)
}

// handleFailure aplica a política on_error sem nunca bloquear o PR, exceto em "fail".
func (s *Service) handleFailure(ctx context.Context, ref github.PullRef, cfg config.Config, out Outcome, cause error, log *slog.Logger) (Outcome, error) {
	reason := describeError(cause)
	log.Error("revisão falhou", "erro", cause, "politica", cfg.OnError)
	out.Failed = reason

	switch cfg.OnError {
	case config.OnErrorFail:
		return out, cause
	case config.OnErrorComment:
		if err := s.upsertSummary(ctx, ref, RenderFailure(cfg.Language, reason), log); err != nil {
			log.Error("não foi possível publicar o aviso de falha", "erro", err)
		}
	}
	return out, nil
}

// describeError resume a causa sem expor detalhes arbitrários em comentário público.
func describeError(err error) string {
	var ae *claude.APIError
	switch {
	case errors.As(err, &ae):
		return fmt.Sprintf("API do Claude: HTTP %d %s", ae.Status, ae.Type)
	case errors.Is(err, claude.ErrTruncated):
		return "resposta da IA cortada por limite de tokens"
	case errors.Is(err, claude.ErrRefusal):
		return "a IA recusou a requisição"
	case errors.Is(err, ErrInvalidResult), errors.Is(err, claude.ErrNoText):
		return "resposta da IA em formato inválido"
	case errors.Is(err, context.DeadlineExceeded):
		return "tempo limite excedido"
	}
	return "erro inesperado; veja os logs"
}
