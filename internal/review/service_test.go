package review

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/claude"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/config"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/diff"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/github"
)

var ref = github.PullRef{Repo: github.Repo{Owner: "acme", Name: "app"}, Number: 7}

type fakeGH struct {
	pr           github.PullRequest
	files        []diff.File
	configFile   string
	configErr    error
	comments     []github.Comment
	reviewCmts   []github.ReviewComment
	getPRErr     error
	listFilesErr error
	reviewErr    error
	createErr    error
	updateErr    error

	gotConfigRef string
	created      []string
	updated      map[int64]string
	reviews      []github.ReviewInput
}

func (f *fakeGH) GetPullRequest(context.Context, github.PullRef) (github.PullRequest, error) {
	return f.pr, f.getPRErr
}
func (f *fakeGH) ListFiles(context.Context, github.PullRef) ([]diff.File, error) {
	return f.files, f.listFilesErr
}
func (f *fakeGH) GetFile(_ context.Context, _ github.Repo, _, r string) ([]byte, bool, error) {
	f.gotConfigRef = r
	if f.configErr != nil {
		return nil, false, f.configErr
	}
	return []byte(f.configFile), f.configFile != "", nil
}
func (f *fakeGH) ListComments(context.Context, github.PullRef) ([]github.Comment, error) {
	return f.comments, nil
}
func (f *fakeGH) CreateComment(_ context.Context, _ github.PullRef, body string) error {
	f.created = append(f.created, body)
	return f.createErr
}
func (f *fakeGH) UpdateComment(_ context.Context, _ github.Repo, id int64, body string) error {
	if f.updated == nil {
		f.updated = map[int64]string{}
	}
	f.updated[id] = body
	return f.updateErr
}
func (f *fakeGH) ListReviewComments(context.Context, github.PullRef) ([]github.ReviewComment, error) {
	return f.reviewCmts, nil
}
func (f *fakeGH) CreateReview(_ context.Context, _ github.PullRef, in github.ReviewInput) error {
	f.reviews = append(f.reviews, in)
	return f.reviewErr
}

type fakeLLM struct {
	text  string
	err   error
	calls int
	got   claude.Request
}

func (f *fakeLLM) Complete(_ context.Context, r claude.Request) (claude.Response, error) {
	f.calls++
	f.got = r
	return claude.Response{Text: f.text, Usage: claude.Usage{InputTokens: 10, OutputTokens: 5}}, f.err
}

const patch = "@@ -1,2 +1,4 @@\n ctx\n+linha2\n+linha3\n ctx2\n"

func newFixture() (*fakeGH, *fakeLLM, *Service) {
	gh := &fakeGH{
		pr:    github.PullRequest{Number: 7, State: "open", HeadSHA: "head123", BaseSHA: "base456", Title: "t", AuthorType: "User"},
		files: []diff.File{{Path: "a.go", Status: "modified", Patch: patch, Additions: 2}, {Path: "go.sum", Status: "modified", Patch: patch}},
	}
	llm := &fakeLLM{text: `{"summary":"ok","risk_level":"high","missing_tests":["t1"],"findings":[
	  {"file":"a.go","line":2,"severity":"critical","category":"bug","message":"bug na linha 2","suggestion":"fix"},
	  {"file":"a.go","line":50,"severity":"warning","category":"style","message":"fora do diff","suggestion":""},
	  {"file":"a.go","line":3,"severity":"info","category":"style","message":"detalhe","suggestion":""}]}`}
	cfg := config.Defaults()
	cfg.AnthropicKey, cfg.GitHubToken = "k", "t"
	svc := NewService(gh, llm, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.nonce = func() string { return "nonce" }
	return gh, llm, svc
}

func TestRunHappyPath(t *testing.T) {
	gh, llm, svc := newFixture()
	out, err := svc.Run(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if out.Risk != "high" || out.Findings != 3 || out.Inline != 2 || out.Truncated || out.Usage.OutputTokens != 5 {
		t.Fatalf("out = %+v", out)
	}
	// Config lida da base, não do head.
	if gh.gotConfigRef != "base456" {
		t.Fatalf("config lida de %q", gh.gotConfigRef)
	}
	// go.sum filtrado: o prompt só contém a.go.
	if !strings.Contains(llm.got.User, "### a.go") || strings.Contains(llm.got.User, "go.sum") {
		t.Fatalf("user prompt:\n%s", llm.got.User)
	}
	if llm.got.MaxTokens != svc.Cfg.MaxOutputTokens || len(llm.got.Schema) == 0 {
		t.Fatalf("req = %+v", llm.got)
	}
	// Inline: linhas 2 e 3 existem no diff; a 50 vai para o resumo.
	if len(gh.reviews) != 1 || gh.reviews[0].CommitID != "head123" || len(gh.reviews[0].Comments) != 2 {
		t.Fatalf("reviews = %+v", gh.reviews)
	}
	if c := gh.reviews[0].Comments[0]; c.Path != "a.go" || c.Line != 2 || !strings.Contains(c.Body, "bug na linha 2") {
		t.Fatalf("primeiro inline deveria ser o mais severo: %+v", c)
	}
	if len(gh.created) != 1 {
		t.Fatalf("created = %d", len(gh.created))
	}
	sum := gh.created[0]
	if !strings.HasPrefix(sum, Marker) || !strings.Contains(sum, "fora do diff") || strings.Contains(sum, "bug na linha 2") ||
		!strings.Contains(sum, "2 de 3") || !strings.Contains(sum, "1 arquivo(s)") {
		t.Fatalf("resumo:\n%s", sum)
	}
}

func TestRunUpdatesExistingSummary(t *testing.T) {
	gh, _, svc := newFixture()
	gh.comments = []github.Comment{{ID: 1, Body: "oi"}, {ID: 9, Body: Marker + "\nantigo"}}
	if _, err := svc.Run(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if len(gh.created) != 0 || !strings.HasPrefix(gh.updated[9], Marker) {
		t.Fatalf("created=%v updated=%v", gh.created, gh.updated)
	}
}

func TestRunUpdateFailureFallsBackToCreate(t *testing.T) {
	gh, _, svc := newFixture()
	gh.comments = []github.Comment{{ID: 9, Body: Marker}}
	gh.updateErr = errors.New("403")
	if _, err := svc.Run(context.Background(), ref); err != nil || len(gh.created) != 1 {
		t.Fatalf("err=%v created=%d", err, len(gh.created))
	}
}

func TestRunDoesNotRepeatInlineComments(t *testing.T) {
	gh, llm, svc := newFixture()
	first := Finding{File: "a.go", Category: CategoryBug, Message: "bug na linha 2"}
	gh.reviewCmts = []github.ReviewComment{{Path: "a.go", Line: 2, Body: inlinePrefix + InlineID(first) + " -->\nantigo"}}
	out, err := svc.Run(context.Background(), ref)
	if err != nil || llm.calls != 1 {
		t.Fatal(err)
	}
	if len(gh.reviews) != 1 || len(gh.reviews[0].Comments) != 1 || gh.reviews[0].Comments[0].Line != 3 {
		t.Fatalf("só o achado novo deveria ser publicado: %+v", gh.reviews)
	}
	if out.Inline != 2 {
		t.Fatalf("inline (incluindo o já existente) = %d", out.Inline)
	}
}

func TestRunNoNewInlineSkipsReview(t *testing.T) {
	gh, _, svc := newFixture()
	for _, f := range []Finding{
		{File: "a.go", Category: CategoryBug, Message: "bug na linha 2"},
		{File: "a.go", Category: CategoryStyle, Message: "detalhe"},
	} {
		gh.reviewCmts = append(gh.reviewCmts, github.ReviewComment{Body: inlinePrefix + InlineID(f) + " -->"})
	}
	if _, err := svc.Run(context.Background(), ref); err != nil || len(gh.reviews) != 0 {
		t.Fatalf("err=%v reviews=%d", err, len(gh.reviews))
	}
}

func TestRunInlineFailureMovesToSummary(t *testing.T) {
	gh, _, svc := newFixture()
	gh.reviewErr = errors.New("422 line invalid")
	out, err := svc.Run(context.Background(), ref)
	if err != nil || out.Inline != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if s := gh.created[0]; !strings.Contains(s, "bug na linha 2") || !strings.Contains(s, "fora do diff") {
		t.Fatalf("todos os achados deveriam estar no resumo:\n%s", s)
	}
}

func TestRunMinSeverity(t *testing.T) {
	gh, _, svc := newFixture()
	svc.Cfg.MinSeverity = config.SeverityWarning
	out, err := svc.Run(context.Background(), ref)
	if err != nil || out.Findings != 2 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if strings.Contains(gh.created[0], "detalhe") {
		t.Fatal("achado info deveria ter sido filtrado")
	}
}

func TestRunRepoConfig(t *testing.T) {
	t.Run("aplica", func(t *testing.T) {
		gh, llm, svc := newFixture()
		gh.configFile = "language: en\nmin_severity: critical\ninstructions: Seja breve.\nignore: ['a.go']\n"
		gh.files = append(gh.files, diff.File{Path: "b.go", Status: "added", Patch: patch})
		out, err := svc.Run(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(llm.got.System, "Seja breve.") || !strings.Contains(llm.got.System, "in en.") {
			t.Fatalf("system:\n%s", llm.got.System)
		}
		if strings.Contains(llm.got.User, "### a.go") || !strings.Contains(llm.got.User, "### b.go") {
			t.Fatalf("ignore do repo não aplicado:\n%s", llm.got.User)
		}
		// min_severity=critical deixa só 1 achado; como a.go foi ignorado, não vai inline.
		if out.Findings != 1 || out.Inline != 0 {
			t.Fatalf("findings=%d inline=%d", out.Findings, out.Inline)
		}
		if !strings.Contains(gh.created[0], "Automated review") {
			t.Fatal("idioma en não aplicado ao comentário")
		}
	})
	t.Run("yaml inválido vira aviso", func(t *testing.T) {
		gh, _, svc := newFixture()
		gh.configFile = "linguagem: pt\n"
		if _, err := svc.Run(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(gh.created[0], "Configuração do repositório ignorada") {
			t.Fatalf("faltou aviso:\n%s", gh.created[0])
		}
	})
	t.Run("valores inválidos viram aviso", func(t *testing.T) {
		gh, _, svc := newFixture()
		gh.configFile = "min_severity: banana\n"
		if _, err := svc.Run(context.Background(), ref); err != nil || !strings.Contains(gh.created[0], "min_severity") {
			t.Fatalf("err=%v\n%s", err, gh.created[0])
		}
	})
	t.Run("erro ao ler segue com padrão", func(t *testing.T) {
		gh, _, svc := newFixture()
		gh.configErr = errors.New("boom")
		if _, err := svc.Run(context.Background(), ref); err != nil || len(gh.created) != 1 {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestRunSkips(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeGH, *Service)
		want   string
	}{
		{"bot", func(g *fakeGH, _ *Service) { g.pr.AuthorType = "Bot" }, "bot"},
		{"label", func(g *fakeGH, _ *Service) { g.pr.Labels = []string{"Skip-Review"} }, "label"},
		{"fechado", func(g *fakeGH, _ *Service) { g.pr.State = "closed" }, "aberto"},
		{"só lockfile", func(g *fakeGH, _ *Service) { g.files = g.files[1:] }, "revisável"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh, llm, svc := newFixture()
			tt.mutate(gh, svc)
			out, err := svc.Run(context.Background(), ref)
			if err != nil || !strings.Contains(out.Skipped, tt.want) {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if llm.calls != 0 || len(gh.created) != 0 {
				t.Fatal("não deveria chamar a IA nem publicar")
			}
		})
	}

	t.Run("bots permitidos quando configurado", func(t *testing.T) {
		gh, _, svc := newFixture()
		gh.pr.AuthorType = "Bot"
		svc.Cfg.SkipBots = false
		if out, _ := svc.Run(context.Background(), ref); out.Skipped != "" {
			t.Fatalf("out = %+v", out)
		}
	})
}

func TestRunTruncationWarning(t *testing.T) {
	gh, _, svc := newFixture()
	big := diff.File{Path: "big.go", Status: "added", Patch: "@@ -0,0 +1 @@\n+" + strings.Repeat("x", 500) + "\n"}
	gh.files = append(gh.files, big)
	svc.Cfg.MaxDiffBytes = len(patch) + len("a.go") + 20
	out, err := svc.Run(context.Background(), ref)
	if err != nil || !out.Truncated {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if s := gh.created[0]; !strings.Contains(s, "big.go") || !strings.Contains(s, "não revisados") {
		t.Fatalf("aviso de truncamento ausente:\n%s", s)
	}
}

func TestRunFailurePolicies(t *testing.T) {
	apiErr := &claude.APIError{Status: 529, Type: "overloaded_error", Message: "segredo-interno"}
	tests := []struct {
		name        string
		policy      config.OnError
		llmErr      error
		llmText     string
		wantErr     bool
		wantComment bool
		wantInBody  string
	}{
		{"comment: API fora", config.OnErrorComment, apiErr, "", false, true, "HTTP 529 overloaded_error"},
		{"silent", config.OnErrorSilent, apiErr, "", false, false, ""},
		{"fail", config.OnErrorFail, apiErr, "", true, false, ""},
		{"comment: truncada", config.OnErrorComment, claude.ErrTruncated, "", false, true, "limite de tokens"},
		{"comment: JSON ruim", config.OnErrorComment, nil, "isto não é json", false, true, "formato inválido"},
		{"comment: erro inesperado", config.OnErrorComment, errors.New("detalhe sensível"), "", false, true, "veja os logs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh, llm, svc := newFixture()
			svc.Cfg.OnError = tt.policy
			llm.err = tt.llmErr
			if tt.llmText != "" {
				llm.text = tt.llmText
			}
			out, err := svc.Run(context.Background(), ref)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if out.Failed == "" {
				t.Fatal("Outcome.Failed deveria estar preenchido")
			}
			if (len(gh.created) == 1) != tt.wantComment {
				t.Fatalf("comentários = %d", len(gh.created))
			}
			if tt.wantComment {
				body := gh.created[0]
				if !strings.HasPrefix(body, Marker) || !strings.Contains(body, tt.wantInBody) ||
					strings.Contains(body, "segredo-interno") || strings.Contains(body, "detalhe sensível") {
					t.Fatalf("corpo:\n%s", body)
				}
			}
		})
	}
}

func TestRunFailureCommentErrorDoesNotBlock(t *testing.T) {
	gh, llm, svc := newFixture()
	llm.err = errors.New("x")
	gh.createErr = errors.New("sem permissão")
	if _, err := svc.Run(context.Background(), ref); err != nil {
		t.Fatalf("falha ao comentar não pode bloquear: %v", err)
	}
}

func TestRunGitHubErrors(t *testing.T) {
	t.Run("não consegue consultar o PR", func(t *testing.T) {
		gh, _, svc := newFixture()
		gh.getPRErr = errors.New("boom")
		if _, err := svc.Run(context.Background(), ref); err == nil {
			t.Fatal("esperava erro")
		}
	})
	t.Run("não consegue listar arquivos: segue política", func(t *testing.T) {
		gh, llm, svc := newFixture()
		gh.listFilesErr = errors.New("boom")
		if _, err := svc.Run(context.Background(), ref); err != nil || llm.calls != 0 || len(gh.created) != 1 {
			t.Fatalf("err=%v calls=%d created=%d", err, llm.calls, len(gh.created))
		}
	})
	t.Run("limite de diff pequeno demais", func(t *testing.T) {
		gh, _, svc := newFixture()
		svc.Cfg.MaxDiffBytes = 3
		if _, err := svc.Run(context.Background(), ref); err != nil || len(gh.created) != 1 {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestDescribeError(t *testing.T) {
	tests := map[string]error{
		"tempo limite":     context.DeadlineExceeded,
		"recusou":          claude.ErrRefusal,
		"formato inválido": claude.ErrNoText,
	}
	for want, err := range tests {
		if got := describeError(err); !strings.Contains(got, want) {
			t.Errorf("describeError(%v) = %q", err, got)
		}
	}
}
