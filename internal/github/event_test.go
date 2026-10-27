package github

import (
	"strings"
	"testing"
)

const eventJSON = `{
  "action": "synchronize",
  "number": 42,
  "pull_request": {"number": 42, "title": "T", "state": "open", "head": {"sha": "deadbeef"},
    "user": {"login": "ana", "type": "User"}, "labels": [{"name": "bug"}]},
  "repository": {"name": "app", "owner": {"login": "acme"}}
}`

func TestParseEvent(t *testing.T) {
	e, err := ParseEvent(strings.NewReader(eventJSON))
	if err != nil {
		t.Fatal(err)
	}
	if e.Ref() != (PullRef{Repo: Repo{Owner: "acme", Name: "app"}, Number: 42}) {
		t.Fatalf("ref = %v", e.Ref())
	}
	pr := e.PR()
	if pr.HeadSHA != "deadbeef" || pr.Author != "ana" || pr.Labels[0] != "bug" {
		t.Fatalf("pr = %+v", pr)
	}
	if !e.Reviewable() {
		t.Fatal("synchronize deve ser revisável")
	}
}

func TestParseEventErrors(t *testing.T) {
	tests := []struct{ name, in string }{
		{"json inválido", `{`},
		{"sem repositório", `{"action":"opened","number":1}`},
		{"sem número", `{"action":"opened","repository":{"name":"a","owner":{"login":"b"}}}`},
		{"grande demais", `{"x":"` + strings.Repeat("a", maxEventBytes) + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseEvent(strings.NewReader(tt.in)); err == nil {
				t.Fatal("esperava erro")
			}
		})
	}
}

func TestParseEventNumberFallback(t *testing.T) {
	e, err := ParseEventBytes([]byte(`{"action":"opened","pull_request":{"number":9},"repository":{"name":"a","owner":{"login":"b"}}}`))
	if err != nil || e.Number != 9 {
		t.Fatalf("e=%+v err=%v", e, err)
	}
}

func TestReviewable(t *testing.T) {
	tests := map[string]bool{
		"opened": true, "synchronize": true, "reopened": true,
		"closed": false, "edited": false, "labeled": false, "": false,
	}
	for action, want := range tests {
		if got := (Event{Action: action}).Reviewable(); got != want {
			t.Errorf("Reviewable(%q) = %v", action, got)
		}
	}
}

func TestVerifySignature(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"a":1}`)
	good := Sign(secret, body)

	tests := []struct {
		name   string
		secret []byte
		body   []byte
		header string
		want   bool
	}{
		{"válida", secret, body, good, true},
		{"corpo alterado", secret, []byte(`{"a":2}`), good, false},
		{"segredo errado", []byte("outro"), body, good, false},
		{"sem prefixo", secret, body, strings.TrimPrefix(good, "sha256="), false},
		{"prefixo sha1", secret, body, "sha1=" + strings.TrimPrefix(good, "sha256="), false},
		{"hex inválido", secret, body, "sha256=zzzz", false},
		{"header vazio", secret, body, "", false},
		{"segredo vazio nunca valida", nil, body, Sign(nil, body), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifySignature(tt.secret, tt.body, tt.header); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSignKnownVector(t *testing.T) {
	// Vetor de exemplo da documentação do GitHub sobre validação de webhooks.
	got := Sign([]byte("It's a Secret to Everybody"), []byte("Hello, World!"))
	want := "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	if got != want {
		t.Fatalf("got %s", got)
	}
}
