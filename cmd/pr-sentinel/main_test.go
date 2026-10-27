package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/config"
)

func TestRun(t *testing.T) {
	noEnv := func(string) string { return "" }
	tests := []struct {
		name    string
		args    []string
		wantErr string
		wantOut string
	}{
		{"sem argumentos", nil, "uso:", ""},
		{"versão", []string{"version"}, "", "dev"},
		{"desconhecido", []string{"foo"}, "desconhecido", ""},
		{"action fora do GitHub Actions ignora o evento", []string{"action"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := run(context.Background(), tt.args, noEnv, &out, &errOut)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Fatalf("stdout = %q", out.String())
			}
		})
	}
}

func TestActionWithoutKeysFails(t *testing.T) {
	env := map[string]string{"GITHUB_EVENT_NAME": "pull_request"}
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"action"}, func(k string) string { return env[k] }, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestServerValidation(t *testing.T) {
	env := map[string]string{"ANTHROPIC_API_KEY": "k", "GITHUB_TOKEN": "t"}
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"server"}, func(k string) string { return env[k] }, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "WEBHOOK_SECRET") {
		t.Fatalf("err = %v", err)
	}
}

func TestServerStartsAndStops(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY": "k", "GITHUB_TOKEN": "t", "WEBHOOK_SECRET": "s", "PR_SENTINEL_ADDR": "127.0.0.1:0",
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out, errOut bytes.Buffer
	go func() {
		done <- run(ctx, []string{"server"}, func(k string) string { return env[k] }, &out, &syncBuf{&errOut})
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("servidor não encerrou")
	}
}

// syncBuf evita corrida entre o logger do servidor e a leitura no teste.
type syncBuf struct{ b *bytes.Buffer }

func (s *syncBuf) Write(p []byte) (int, error) { return len(p), nil }

func TestBuildService(t *testing.T) {
	svc := buildService(func(string) string { return "" }, nil)(config.Defaults())
	if svc == nil {
		t.Fatal("service nulo")
	}
}
