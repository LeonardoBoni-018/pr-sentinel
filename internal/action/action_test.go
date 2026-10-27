package action

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
	"github.com/LeonardoBoni-018/pr-sentinel/internal/review"
)

type fakeGH struct {
	created []string
	prCalls int
}

func (f *fakeGH) GetPullRequest(context.Context, github.PullRef) (github.PullRequest, error) {
	f.prCalls++
	return github.PullRequest{State: "open", HeadSHA: "h", BaseSHA: "b"}, nil
}
func (f *fakeGH) ListFiles(context.Context, github.PullRef) ([]diff.File, error) {
	return []diff.File{{Path: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n+x\n"}}, nil
}
func (f *fakeGH) GetFile(context.Context, github.Repo, string, string) ([]byte, bool, error) {
	return nil, false, nil
}
func (f *fakeGH) ListComments(context.Context, github.PullRef) ([]github.Comment, error) {
	return nil, nil
}
func (f *fakeGH) CreateComment(_ context.Context, _ github.PullRef, b string) error {
	f.created = append(f.created, b)
	return nil
}
func (f *fakeGH) UpdateComment(context.Context, github.Repo, int64, string) error { return nil }
func (f *fakeGH) ListReviewComments(context.Context, github.PullRef) ([]github.ReviewComment, error) {
	return nil, nil
}
func (f *fakeGH) CreateReview(context.Context, github.PullRef, github.ReviewInput) error { return nil }

type fakeLLM struct{ err error }

func (f fakeLLM) Complete(context.Context, claude.Request) (claude.Response, error) {
	return claude.Response{Text: `{"summary":"ok","risk_level":"low","findings":[],"missing_tests":[]}`}, f.err
}

const eventJSON = `{"action":"%s","number":3,"pull_request":{"number":3,"state":"open"},
 "repository":{"name":"app","owner":{"login":"acme"}}}`

func deps(t *testing.T, env map[string]string, event string, gh *fakeGH, llm fakeLLM) Deps {
	t.Helper()
	return Deps{
		Getenv: func(k string) string { return env[k] },
		ReadFile: func(p string) ([]byte, error) {
			if p != env["GITHUB_EVENT_PATH"] {
				return nil, errors.New("arquivo inexistente")
			}
			return []byte(event), nil
		},
		Build: func(cfg config.Config) *review.Service {
			return review.NewService(gh, llm, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func baseEnv() map[string]string {
	return map[string]string{
		"GITHUB_EVENT_NAME": "pull_request", "GITHUB_EVENT_PATH": "/event.json",
		"ANTHROPIC_API_KEY": "k", "GITHUB_TOKEN": "t",
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name      string
		mutateEnv func(map[string]string)
		action    string
		llm       fakeLLM
		wantErr   string
		wantPosts int
	}{
		{name: "revisa PR aberto", action: "opened", wantPosts: 1},
		{name: "synchronize", action: "synchronize", wantPosts: 1},
		{name: "ação irrelevante", action: "labeled"},
		{name: "evento que não é PR", action: "opened", mutateEnv: func(e map[string]string) { e["GITHUB_EVENT_NAME"] = "push" }},
		{name: "pull_request_target", action: "opened", mutateEnv: func(e map[string]string) { e["GITHUB_EVENT_NAME"] = "pull_request_target" }, wantPosts: 1},
		{name: "sem chave", action: "opened", mutateEnv: func(e map[string]string) { delete(e, "ANTHROPIC_API_KEY") }, wantErr: "ANTHROPIC_API_KEY"},
		{name: "env inválida", action: "opened", mutateEnv: func(e map[string]string) { e["PR_SENTINEL_MAX_DIFF_BYTES"] = "x" }, wantErr: "MAX_DIFF_BYTES"},
		{name: "sem caminho do evento", action: "opened", mutateEnv: func(e map[string]string) { delete(e, "GITHUB_EVENT_PATH") }, wantErr: "GITHUB_EVENT_PATH"},
		{name: "IA falha, política comment: sucesso", action: "opened", llm: fakeLLM{err: errors.New("x")}, wantPosts: 1},
		{
			name: "IA falha, política fail: erro", action: "opened", llm: fakeLLM{err: errors.New("x")}, wantErr: "chamando a IA",
			mutateEnv: func(e map[string]string) { e["PR_SENTINEL_ON_ERROR"] = "fail" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv()
			if tt.mutateEnv != nil {
				tt.mutateEnv(env)
			}
			gh := &fakeGH{}
			err := Run(context.Background(), deps(t, env, strings.Replace(eventJSON, "%s", tt.action, 1), gh, tt.llm))
			if tt.wantErr == "" && err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, quer %q", err, tt.wantErr)
			}
			if len(gh.created) != tt.wantPosts {
				t.Fatalf("comentários = %d, quer %d", len(gh.created), tt.wantPosts)
			}
		})
	}
}

func TestRunBadEventFile(t *testing.T) {
	env := baseEnv()
	gh := &fakeGH{}
	if err := Run(context.Background(), deps(t, env, `{"action":`, gh, fakeLLM{})); err == nil {
		t.Fatal("JSON inválido deveria falhar")
	}
	if err := Run(context.Background(), deps(t, env, `{"action":"opened"}`, gh, fakeLLM{})); err == nil {
		t.Fatal("evento sem repositório deveria falhar")
	}
	env["GITHUB_EVENT_PATH"] = "/outro"
	d := deps(t, env, "{}", gh, fakeLLM{})
	d.ReadFile = func(string) ([]byte, error) { return nil, errors.New("não existe") }
	if err := Run(context.Background(), d); err == nil {
		t.Fatal("arquivo ilegível deveria falhar")
	}
	if gh.prCalls != 0 {
		t.Fatal("não deveria consultar o GitHub com evento inválido")
	}
}
