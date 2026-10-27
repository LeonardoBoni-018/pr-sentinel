// Package server implementa o modo servidor: recebe webhooks do GitHub,
// valida a assinatura HMAC e enfileira as revisões de PR.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/config"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/github"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/review"
)

const (
	maxBodyBytes    = 5 << 20
	defaultWorkers  = 4
	defaultQueue    = 64
	defaultJobLimit = 10 * time.Minute
	shutdownGrace   = 30 * time.Second
)

// Reviewer é o que o servidor precisa do núcleo (satisfeito por *review.Service).
type Reviewer interface {
	Run(ctx context.Context, ref github.PullRef) (review.Outcome, error)
}

// Options configura o Server.
type Options struct {
	Secret   config.Secret // segredo do webhook
	Reviewer Reviewer
	Log      *slog.Logger
	Version  string
	Workers  int           // padrão 4
	Queue    int           // tamanho da fila; padrão 64
	JobLimit time.Duration // tempo máximo por revisão; padrão 10 min
}

// Server recebe webhooks e processa revisões em segundo plano.
type Server struct {
	opts Options
	log  *slog.Logger
	jobs chan github.PullRef

	mu    sync.Mutex
	state map[github.PullRef]*prState // PRs com revisão enfileirada ou em andamento
	wg    sync.WaitGroup
}

// prState evita revisões simultâneas do mesmo PR: pushes em sequência rápida
// são fundidos em uma única reexecução ao final da atual.
type prState struct{ pending bool }

// New cria o servidor.
func New(o Options) *Server {
	if o.Workers <= 0 {
		o.Workers = defaultWorkers
	}
	if o.Queue <= 0 {
		o.Queue = defaultQueue
	}
	if o.JobLimit <= 0 {
		o.JobLimit = defaultJobLimit
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &Server{opts: o, log: log, jobs: make(chan github.PullRef, o.Queue), state: map[github.PullRef]*prState{}}
}

// Handler devolve as rotas: GET /healthz e POST /webhook.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /webhook", s.webhook)
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": s.opts.Version})
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "payload grande demais", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "erro lendo o corpo", http.StatusBadRequest)
		return
	}

	// A assinatura vem antes de qualquer outra decisão: nada de não autenticado é processado.
	if !github.VerifySignature([]byte(s.opts.Secret.Reveal()), body, r.Header.Get(github.SignatureHeader)) {
		s.log.Warn("webhook rejeitado: assinatura inválida", "remote", r.RemoteAddr)
		http.Error(w, "assinatura inválida", http.StatusUnauthorized)
		return
	}

	log := s.log.With("delivery", r.Header.Get("X-GitHub-Delivery"))
	switch ev := r.Header.Get("X-GitHub-Event"); ev {
	case "ping":
		writeJSON(w, http.StatusOK, "pong")
		return
	case "pull_request":
	default:
		log.Info("evento ignorado", "evento", ev)
		writeJSON(w, http.StatusOK, "ignored")
		return
	}

	event, err := github.ParseEventBytes(body)
	if err != nil {
		log.Warn("evento inválido", "erro", err)
		http.Error(w, "evento inválido", http.StatusBadRequest)
		return
	}
	if !event.Reviewable() {
		log.Info("ação ignorada", "acao", event.Action)
		writeJSON(w, http.StatusOK, "ignored")
		return
	}

	ref := event.Ref()
	if !s.enqueue(ref) {
		log.Warn("fila cheia; revisão descartada", "pr", ref.String())
		w.Header().Set("Retry-After", "60")
		http.Error(w, "fila cheia", http.StatusServiceUnavailable)
		return
	}
	log.Info("revisão enfileirada", "pr", ref.String(), "acao", event.Action)
	writeJSON(w, http.StatusAccepted, "queued")
}

func writeJSON(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": msg})
}

// enqueue agenda a revisão. Se o PR já está na fila ou em execução, apenas
// marca uma reexecução pendente.
func (s *Server) enqueue(ref github.PullRef) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.state[ref]; ok {
		st.pending = true
		return true
	}
	select {
	case s.jobs <- ref:
		s.state[ref] = &prState{}
		return true
	default:
		return false
	}
}

func (s *Server) worker(ctx context.Context) {
	defer s.wg.Done()
	for ref := range s.jobs {
		s.process(ctx, ref)
	}
}

func (s *Server) process(ctx context.Context, ref github.PullRef) {
	for {
		s.runOnce(ctx, ref)

		s.mu.Lock()
		st := s.state[ref]
		if st.pending && ctx.Err() == nil {
			st.pending = false
			s.mu.Unlock()
			continue
		}
		delete(s.state, ref)
		s.mu.Unlock()
		return
	}
}

func (s *Server) runOnce(ctx context.Context, ref github.PullRef) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.JobLimit)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("pânico durante a revisão", "pr", ref.String(), "panic", p)
		}
	}()
	if _, err := s.opts.Reviewer.Run(ctx, ref); err != nil {
		s.log.Error("revisão falhou", "pr", ref.String(), "erro", err)
	}
}

// Serve atende em ln até ctx ser cancelado e então encerra com graça:
// para de aceitar conexões e espera as revisões em andamento.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	jobCtx, cancelJobs := context.WithCancel(context.Background())
	defer cancelJobs()
	for i := 0; i < s.opts.Workers; i++ {
		s.wg.Add(1)
		go s.worker(jobCtx)
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.Background() },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.log.Info("servidor iniciado", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		cancelJobs()
		close(s.jobs)
		s.wg.Wait()
		return err
	case <-ctx.Done():
	}

	s.log.Info("encerrando servidor")
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := srv.Shutdown(shutCtx)
	close(s.jobs) // nenhum handler ativo restou, então ninguém mais envia

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutCtx.Done():
		cancelJobs()
		<-done
	}
	return err
}
