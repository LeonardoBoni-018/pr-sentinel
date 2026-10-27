package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

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

func TestBuildService(t *testing.T) {
	svc := buildService(func(string) string { return "" }, nil)(config.Defaults())
	if svc == nil {
		t.Fatal("service nulo")
	}
}
