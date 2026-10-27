package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const okBody = `{"content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"{\"ok\":true}"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`

const secret = "sk-ant-segredo-123"

type reply struct {
	status  int
	body    string
	headers map[string]string
}

// newServer devolve as respostas em sequência (a última se repete).
func newServer(t *testing.T, replies ...reply) (*httptest.Server, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&n, 1)) - 1
		if i >= len(replies) {
			i = len(replies) - 1
		}
		rp := replies[i]
		for k, v := range rp.headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(rp.status)
		_, _ = io.WriteString(w, rp.body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func newClient(url string, sleeps *[]time.Duration) *Client {
	c := New(Options{APIKey: secret, Model: "claude-sonnet-5-5", BaseURL: url, MaxRetries: 3, BaseDelay: time.Second, MaxDelay: 8 * time.Second})
	c.rand = func() float64 { return 1 } // sem jitter: espera = d
	c.sleep = func(_ context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return nil
	}
	return c
}

var req = Request{System: "sys", User: "usr", MaxTokens: 100, Schema: json.RawMessage(`{"type":"object"}`)}

func TestCompleteSuccess(t *testing.T) {
	var gotHeaders http.Header
	var gotBody map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders, gotPath = r.Header.Clone(), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("request-id", "req_1")
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	var sleeps []time.Duration
	resp, err := newClient(srv.URL, &sleeps).Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"ok":true}` || resp.StopReason != "end_turn" || resp.RequestID != "req_1" || resp.Usage.OutputTokens != 5 {
		t.Fatalf("resp = %+v", resp)
	}
	if gotPath != "/v1/messages" || gotHeaders.Get("x-api-key") != secret ||
		gotHeaders.Get("anthropic-version") != "2023-06-01" || gotHeaders.Get("content-type") != "application/json" {
		t.Fatalf("path=%s headers=%v", gotPath, gotHeaders)
	}
	if gotBody["model"] != "claude-sonnet-5-5" || gotBody["max_tokens"] != float64(100) || gotBody["system"] != "sys" {
		t.Fatalf("body = %v", gotBody)
	}
	msgs := gotBody["messages"].([]any)
	if m := msgs[0].(map[string]any); m["role"] != "user" || m["content"] != "usr" {
		t.Fatalf("messages = %v", msgs)
	}
	f := gotBody["output_config"].(map[string]any)["format"].(map[string]any)
	if f["type"] != "json_schema" || f["schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("format = %v", f)
	}
	if _, ok := gotBody["tool_choice"]; ok {
		t.Fatal("tool_choice forçado não é suportado pelo modelo padrão")
	}
	if len(sleeps) != 0 {
		t.Fatalf("não deveria dormir: %v", sleeps)
	}
}

func TestCompleteWithoutSchemaOmitsOutputConfig(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()
	var sleeps []time.Duration
	if _, err := newClient(srv.URL, &sleeps).Complete(context.Background(), Request{User: "x", MaxTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := gotBody["output_config"]; ok {
		t.Fatal("output_config deveria ser omitido")
	}
}

func TestCompleteRetries(t *testing.T) {
	overloaded := reply{status: 529, body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}
	tests := []struct {
		name       string
		replies    []reply
		wantCalls  int32
		wantSleeps []time.Duration
		wantErr    string
	}{
		{
			name:       "529 duas vezes e depois sucesso (backoff exponencial)",
			replies:    []reply{overloaded, overloaded, {status: 200, body: okBody}},
			wantCalls:  3,
			wantSleeps: []time.Duration{time.Second, 2 * time.Second},
		},
		{
			name:       "429 respeita retry-after",
			replies:    []reply{{status: 429, body: `{"error":{"type":"rate_limit_error","message":"slow"}}`, headers: map[string]string{"retry-after": "7"}}, {status: 200, body: okBody}},
			wantCalls:  2,
			wantSleeps: []time.Duration{7 * time.Second},
		},
		{
			name:       "500 repete",
			replies:    []reply{{status: 500, body: `{}`}, {status: 200, body: okBody}},
			wantCalls:  2,
			wantSleeps: []time.Duration{time.Second},
		},
		{
			name:       "esgota tentativas com teto de backoff",
			replies:    []reply{overloaded},
			wantCalls:  4,
			wantSleeps: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
			wantErr:    "overloaded_error",
		},
		{
			name:      "429 de limite de gasto não repete",
			replies:   []reply{{status: 429, body: `{"error":{"type":"rate_limit_error","message":"cap","details":{"error_code":"enforced_spend_limit_reached"}}}`}},
			wantCalls: 1,
			wantErr:   "cap",
		},
		{
			name:      "retry-after maior que o limite desiste",
			replies:   []reply{{status: 429, body: `{}`, headers: map[string]string{"retry-after": "3600"}}},
			wantCalls: 1,
			wantErr:   "429",
		},
		{
			name:      "401 não repete",
			replies:   []reply{{status: 401, body: `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`}},
			wantCalls: 1,
			wantErr:   "authentication_error",
		},
		{
			name:      "400 não repete e corpo não-JSON é tolerado",
			replies:   []reply{{status: 400, body: `<html>bad</html>`}},
			wantCalls: 1,
			wantErr:   "400",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls := newServer(t, tt.replies...)
			var sleeps []time.Duration
			_, err := newClient(srv.URL, &sleeps).Complete(context.Background(), req)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, quer conter %q", err, tt.wantErr)
			}
			if *calls != tt.wantCalls {
				t.Fatalf("chamadas = %d, quer %d", *calls, tt.wantCalls)
			}
			if len(sleeps) != len(tt.wantSleeps) {
				t.Fatalf("sleeps = %v, quer %v", sleeps, tt.wantSleeps)
			}
			for i := range sleeps {
				if sleeps[i] != tt.wantSleeps[i] {
					t.Fatalf("sleeps = %v, quer %v", sleeps, tt.wantSleeps)
				}
			}
		})
	}
}

func TestAPIErrorFields(t *testing.T) {
	srv, _ := newServer(t, reply{status: 401, body: `{"type":"error","error":{"type":"authentication_error","message":"nope"},"request_id":"req_x"}`})
	var sleeps []time.Duration
	_, err := newClient(srv.URL, &sleeps).Complete(context.Background(), req)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 || ae.Type != "authentication_error" || ae.RequestID != "req_x" {
		t.Fatalf("err = %#v", err)
	}
}

func TestJitterWithinBounds(t *testing.T) {
	c := New(Options{BaseDelay: 4 * time.Second, MaxDelay: time.Minute})
	for _, r := range []float64{0, 0.5, 0.999} {
		c.rand = func() float64 { return r }
		d := c.backoff(0, errors.New("x"))
		if d < 2*time.Second || d > 4*time.Second {
			t.Fatalf("jitter fora do intervalo: %v (r=%v)", d, r)
		}
	}
	c.rand = func() float64 { return 1 }
	if d := c.backoff(20, errors.New("x")); d != time.Minute {
		t.Fatalf("teto não aplicado: %v", d)
	}
}

func TestNetworkErrorRetriedAndKeyNotLeaked(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close() // derruba a conexão
			return
		}
		_, _ = io.WriteString(w, okBody)
	}))
	defer srv.Close()

	var sleeps []time.Duration
	resp, err := newClient(srv.URL, &sleeps).Complete(context.Background(), req)
	if err != nil || resp.Text == "" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if n != 2 || len(sleeps) != 1 {
		t.Fatalf("n=%d sleeps=%v", n, sleeps)
	}

	// Servidor inexistente: erro final de rede sem vazar a chave.
	srv.Close()
	_, err = newClient(srv.URL, &sleeps).Complete(context.Background(), req)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnusableResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{"max_tokens", `{"content":[{"type":"text","text":"{\"a\":"}],"stop_reason":"max_tokens"}`, ErrTruncated},
		{"refusal", `{"content":[{"type":"text","text":"não"}],"stop_reason":"refusal"}`, ErrRefusal},
		{"sem texto", `{"content":[{"type":"thinking","thinking":"x"}],"stop_reason":"end_turn"}`, ErrNoText},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls := newServer(t, reply{status: 200, body: tt.body})
			var sleeps []time.Duration
			resp, err := newClient(srv.URL, &sleeps).Complete(context.Background(), req)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, quer %v", err, tt.want)
			}
			if *calls != 1 {
				t.Fatalf("não deveria repetir, chamadas = %d", *calls)
			}
			if resp.StopReason == "" {
				t.Fatal("resposta parcial deveria ser devolvida para logging de uso")
			}
		})
	}

	t.Run("JSON inválido", func(t *testing.T) {
		srv, _ := newServer(t, reply{status: 200, body: `nope`})
		var sleeps []time.Duration
		if _, err := newClient(srv.URL, &sleeps).Complete(context.Background(), req); err == nil {
			t.Fatal("esperava erro")
		}
	})
}

func TestEncodeValidation(t *testing.T) {
	var sleeps []time.Duration
	if _, err := newClient("http://x", &sleeps).Complete(context.Background(), Request{User: "x"}); err == nil {
		t.Fatal("MaxTokens zero deve falhar")
	}
}

func TestContextCancelStopsRetry(t *testing.T) {
	srv, calls := newServer(t, reply{status: 529, body: `{}`})
	ctx, cancel := context.WithCancel(context.Background())
	c := New(Options{APIKey: secret, Model: "m", BaseURL: srv.URL, MaxRetries: 5})
	c.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	_, err := c.Complete(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if *calls != 1 {
		t.Fatalf("chamadas = %d", *calls)
	}
}

func TestTimeoutPerAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	c := New(Options{APIKey: secret, Model: "m", BaseURL: srv.URL, Timeout: 50 * time.Millisecond, MaxRetries: -1})
	start := time.Now()
	_, err := c.Complete(context.Background(), req)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err=%v dur=%v", err, time.Since(start))
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
