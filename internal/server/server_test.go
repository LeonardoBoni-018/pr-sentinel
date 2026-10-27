package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/github"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/review"
)

const secret = "whsec"

type fakeReviewer struct {
	mu    sync.Mutex
	refs  []github.PullRef
	calls int32
	block chan struct{} // se não nulo, Run espera ser liberado
	err   error
	boom  bool
}

func (f *fakeReviewer) Run(ctx context.Context, ref github.PullRef) (review.Outcome, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.refs = append(f.refs, ref)
	f.mu.Unlock()
	if f.boom {
		panic("explodiu")
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
	}
	return review.Outcome{}, f.err
}

func (f *fakeReviewer) count() int { return int(atomic.LoadInt32(&f.calls)) }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func event(action string, n int) string {
	return strings.NewReplacer("ACTION", action, "NUM", string(rune('0'+n))).Replace(
		`{"action":"ACTION","number":NUM,"pull_request":{"number":NUM},"repository":{"name":"app","owner":{"login":"acme"}}}`)
}

func post(t *testing.T, h http.Handler, body, eventName, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", eventName)
	if sig != "" {
		req.Header.Set(github.SignatureHeader, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func signed(t *testing.T, h http.Handler, body, eventName string) *httptest.ResponseRecorder {
	return post(t, h, body, eventName, github.Sign([]byte(secret), []byte(body)))
}

func newServer(r Reviewer, mut ...func(*Options)) *Server {
	o := Options{Secret: secret, Reviewer: r, Log: quiet(), Version: "test"}
	for _, m := range mut {
		m(&o)
	}
	return New(o)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timeout esperando condição")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	newServer(&fakeReviewer{}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) || !strings.Contains(rec.Body.String(), "test") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestWebhookRouting(t *testing.T) {
	h := newServer(&fakeReviewer{}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /webhook = %d", rec.Code)
	}
}

func TestWebhookResponses(t *testing.T) {
	body := event("opened", 1)
	tests := []struct {
		name     string
		do       func(h http.Handler) *httptest.ResponseRecorder
		want     int
		wantRuns bool
	}{
		{"sem assinatura", func(h http.Handler) *httptest.ResponseRecorder { return post(t, h, body, "pull_request", "") }, 401, false},
		{"assinatura errada", func(h http.Handler) *httptest.ResponseRecorder {
			return post(t, h, body, "pull_request", github.Sign([]byte("outro"), []byte(body)))
		}, 401, false},
		{"corpo adulterado", func(h http.Handler) *httptest.ResponseRecorder {
			return post(t, h, body+" ", "pull_request", github.Sign([]byte(secret), []byte(body)))
		}, 401, false},
		{"ping assinado", func(h http.Handler) *httptest.ResponseRecorder { return signed(t, h, `{"zen":"x"}`, "ping") }, 200, false},
		{"ping sem assinatura", func(h http.Handler) *httptest.ResponseRecorder { return post(t, h, `{}`, "ping", "") }, 401, false},
		{"outro evento", func(h http.Handler) *httptest.ResponseRecorder { return signed(t, h, `{}`, "issues") }, 200, false},
		{"ação irrelevante", func(h http.Handler) *httptest.ResponseRecorder {
			return signed(t, h, event("closed", 1), "pull_request")
		}, 200, false},
		{"json inválido", func(h http.Handler) *httptest.ResponseRecorder { return signed(t, h, `{`, "pull_request") }, 400, false},
		{"opened", func(h http.Handler) *httptest.ResponseRecorder { return signed(t, h, body, "pull_request") }, 202, true},
		{"synchronize", func(h http.Handler) *httptest.ResponseRecorder {
			return signed(t, h, event("synchronize", 2), "pull_request")
		}, 202, true},
		{"reopened", func(h http.Handler) *httptest.ResponseRecorder {
			return signed(t, h, event("reopened", 3), "pull_request")
		}, 202, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeReviewer{}
			s := newServer(fr)
			ctx, cancel := context.WithCancel(context.Background())
			s.wg.Add(1)
			go s.worker(ctx)
			defer func() { cancel(); close(s.jobs); s.wg.Wait() }()

			rec := tt.do(s.Handler())
			if rec.Code != tt.want {
				t.Fatalf("status = %d, quer %d (%s)", rec.Code, tt.want, rec.Body)
			}
			if tt.wantRuns {
				waitFor(t, func() bool { return fr.count() == 1 })
				if fr.refs[0].Repo != (github.Repo{Owner: "acme", Name: "app"}) {
					t.Fatalf("ref = %v", fr.refs[0])
				}
			} else {
				time.Sleep(20 * time.Millisecond)
				if fr.count() != 0 {
					t.Fatal("não deveria revisar")
				}
			}
		})
	}
}

func TestWebhookSecretEmptyAlwaysRejects(t *testing.T) {
	s := newServer(&fakeReviewer{}, func(o *Options) { o.Secret = "" })
	body := event("opened", 1)
	if rec := post(t, s.Handler(), body, "pull_request", github.Sign(nil, []byte(body))); rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestWebhookBodyTooLarge(t *testing.T) {
	s := newServer(&fakeReviewer{})
	big := strings.Repeat("a", maxBodyBytes+10)
	if rec := post(t, s.Handler(), big, "pull_request", "sha256=00"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestQueueFull(t *testing.T) {
	// Sem workers rodando: a fila de tamanho 1 enche e o segundo PR é recusado.
	s := newServer(&fakeReviewer{}, func(o *Options) { o.Queue = 1 })
	if rec := signed(t, s.Handler(), event("opened", 1), "pull_request"); rec.Code != 202 {
		t.Fatalf("1º = %d", rec.Code)
	}
	rec := signed(t, s.Handler(), event("opened", 2), "pull_request")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("2º = %d", rec.Code)
	}
	// O mesmo PR do primeiro é aceito (vira reexecução pendente) mesmo com a fila cheia.
	if rec := signed(t, s.Handler(), event("synchronize", 1), "pull_request"); rec.Code != 202 {
		t.Fatalf("mesmo PR = %d", rec.Code)
	}
}

func TestCoalescesPushesForSamePR(t *testing.T) {
	fr := &fakeReviewer{block: make(chan struct{})}
	s := newServer(fr)
	ctx, cancel := context.WithCancel(context.Background())
	s.wg.Add(1)
	go s.worker(ctx)
	defer func() { cancel(); close(s.jobs); s.wg.Wait() }()

	h := s.Handler()
	signed(t, h, event("opened", 1), "pull_request")
	waitFor(t, func() bool { return fr.count() == 1 })
	// Três pushes enquanto a revisão roda: viram UMA reexecução.
	for i := 0; i < 3; i++ {
		if rec := signed(t, h, event("synchronize", 1), "pull_request"); rec.Code != 202 {
			t.Fatalf("push %d = %d", i, rec.Code)
		}
	}
	close(fr.block)
	waitFor(t, func() bool { return fr.count() == 2 })
	time.Sleep(50 * time.Millisecond)
	if fr.count() != 2 {
		t.Fatalf("revisões = %d, quer 2", fr.count())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state) != 0 {
		t.Fatalf("estado vazou: %v", s.state)
	}
}

func TestReviewerErrorAndPanicDoNotKillWorker(t *testing.T) {
	for name, fr := range map[string]*fakeReviewer{
		"erro":   {err: errors.New("falhou")},
		"pânico": {boom: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(fr)
			ctx, cancel := context.WithCancel(context.Background())
			s.wg.Add(1)
			go s.worker(ctx)
			defer func() { cancel(); close(s.jobs); s.wg.Wait() }()

			signed(t, s.Handler(), event("opened", 1), "pull_request")
			waitFor(t, func() bool { return fr.count() == 1 })
			signed(t, s.Handler(), event("opened", 2), "pull_request") // PR diferente: worker ainda vivo
			waitFor(t, func() bool { return fr.count() == 2 })
		})
	}
}

func TestServeAndGracefulShutdown(t *testing.T) {
	fr := &fakeReviewer{block: make(chan struct{})}
	s := newServer(fr)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("sem rede local:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	base := "http://" + ln.Addr().String()
	waitFor(t, func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	})

	body := event("opened", 1)
	req, _ := http.NewRequest(http.MethodPost, base+"/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set(github.SignatureHeader, github.Sign([]byte(secret), []byte(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 202 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	_ = resp.Body.Close()
	waitFor(t, func() bool { return fr.count() == 1 })

	cancel()
	select {
	case <-done:
		t.Fatal("deveria esperar a revisão em andamento")
	case <-time.After(100 * time.Millisecond):
	}
	close(fr.block)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve não encerrou")
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("sem rede local:", err)
	}
	_ = ln.Close() // listener fechado: Serve falha imediatamente
	if err := newServer(&fakeReviewer{}).Serve(context.Background(), ln); err == nil {
		t.Fatal("esperava erro")
	}
}
