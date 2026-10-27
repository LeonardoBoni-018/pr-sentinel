package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseRepoFile(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    RepoFile
		wantErr bool
	}{
		{"vazio", "", RepoFile{}, false},
		{"só comentário", "# nada\n", RepoFile{}, false},
		{
			"completo",
			"language: en\nmin_severity: warning\non_error: silent\nignore:\n  - \"docs/**\"\n  - \"*.md\"\ninstructions: |\n  Foque em segurança.\n",
			RepoFile{Language: "en", MinSeverity: "warning", OnError: "silent", Ignore: []string{"docs/**", "*.md"}, Instructions: "Foque em segurança.\n"},
			false,
		},
		{"campo desconhecido", "languagee: en\n", RepoFile{}, true},
		{"yaml quebrado", "language: [\n", RepoFile{}, true},
		{"grande demais", "instructions: " + strings.Repeat("a", maxRepoFileBytes), RepoFile{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRepoFile(strings.NewReader(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestApply(t *testing.T) {
	base := Defaults()
	base.Ignore = []string{"vendor/**"}

	got := base.Apply(RepoFile{Language: "es", MinSeverity: "critical", OnError: "fail", Instructions: "x", Ignore: []string{"gen/**"}})
	if got.Language != "es" || got.MinSeverity != SeverityCritical || got.OnError != OnErrorFail || got.Instructions != "x" {
		t.Fatalf("got %+v", got)
	}
	if !reflect.DeepEqual(got.Ignore, []string{"vendor/**", "gen/**"}) {
		t.Fatalf("ignore = %v", got.Ignore)
	}

	same := base.Apply(RepoFile{})
	if !reflect.DeepEqual(same, base) {
		t.Fatal("RepoFile vazio não deve alterar a config")
	}
	if len(base.Ignore) != 1 {
		t.Fatal("Apply não pode mutar o slice original")
	}
}
