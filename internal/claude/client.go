// Package claude é um cliente mínimo da Messages API da Anthropic, escrito
// sobre net/http, com timeout, retry com backoff exponencial e tratamento de
// rate limit.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/config"
)

const (
	// DefaultBaseURL é o endpoint público da API.
	DefaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"

	maxResponseBytes = 8 << 20
	// maxWait é o maior intervalo de espera aceito antes de desistir do retry.
	maxWait = 60 * time.Second
)

// Request é uma chamada de revisão: prompts e, opcionalmente, um JSON Schema
// que restringe a saída (output_config.format).
type Request struct {
	System    string
	User      string
	MaxTokens int
	Schema    json.RawMessage
}

// Usage reporta o consumo de tokens.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response é o texto gerado e metadados da chamada.
type Response struct {
	Text       string
	StopReason string
	Usage      Usage
	RequestID  string
}

// Completer é a interface consumida pelo núcleo e facilmente simulada em testes.
type Completer interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Erros sentinela para respostas que chegaram, mas não são utilizáveis.
var (
	ErrTruncated = errors.New("resposta cortada por max_tokens")
	ErrRefusal   = errors.New("o modelo recusou a requisição")
	ErrNoText    = errors.New("resposta sem bloco de texto")
)

// Options configura o Client.
type Options struct {
	APIKey     config.Secret
	Model      string
	BaseURL    string        // padrão: DefaultBaseURL
	Timeout    time.Duration // por tentativa; padrão 120s
	MaxRetries int           // tentativas extras após a primeira; padrão 3
	BaseDelay  time.Duration // primeiro backoff; padrão 1s
	MaxDelay   time.Duration // teto do backoff; padrão 30s
	UserAgent  string
	HTTPClient *http.Client
}

// Client implementa Completer.
type Client struct {
	opts  Options
	http  *http.Client
	sleep func(context.Context, time.Duration) error
	rand  func() float64
}

// New cria um Client aplicando os padrões.
func New(o Options) *Client {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.Timeout <= 0 {
		o.Timeout = 120 * time.Second
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	} else if o.MaxRetries == 0 {
		o.MaxRetries = 3
	}
	if o.BaseDelay <= 0 {
		o.BaseDelay = time.Second
	}
	if o.MaxDelay <= 0 {
		o.MaxDelay = 30 * time.Second
	}
	if o.UserAgent == "" {
		o.UserAgent = "pr-sentinel"
	}
	h := o.HTTPClient
	if h == nil {
		h = &http.Client{}
	}
	return &Client{opts: o, http: h, sleep: sleepCtx, rand: rand.Float64}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type apiRequest struct {
	Model        string           `json:"model"`
	MaxTokens    int              `json:"max_tokens"`
	System       string           `json:"system,omitempty"`
	Messages     []apiMessage     `json:"messages"`
	OutputConfig *apiOutputConfig `json:"output_config,omitempty"`
}

type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiOutputConfig struct {
	Format apiFormat `json:"format"`
}

type apiFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type apiResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      Usage  `json:"usage"`
}

// Complete envia a requisição, repetindo em falhas transitórias (429 com
// retry-after, 5xx, 529 e erros de rede).
func (c *Client) Complete(ctx context.Context, req Request) (Response, error) {
	body, err := c.encode(req)
	if err != nil {
		return Response{}, err
	}

	var (
		lastErr  error
		lastResp Response // parcial em respostas inutilizáveis, útil para logar uso
	)
	for attempt := 0; attempt <= c.opts.MaxRetries; attempt++ {
		resp, err := c.once(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastResp, lastErr = resp, err
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		if !retryable(err) || attempt == c.opts.MaxRetries {
			break
		}
		wait := c.backoff(attempt, err)
		if wait > maxWait {
			break
		}
		if err := c.sleep(ctx, wait); err != nil {
			return Response{}, err
		}
	}
	return lastResp, lastErr
}

func (c *Client) encode(req Request) ([]byte, error) {
	if req.MaxTokens <= 0 {
		return nil, errors.New("claude: MaxTokens deve ser positivo")
	}
	ar := apiRequest{
		Model:     c.opts.Model,
		MaxTokens: req.MaxTokens,
		System:    req.System,
		Messages:  []apiMessage{{Role: "user", Content: req.User}},
	}
	if len(req.Schema) > 0 {
		ar.OutputConfig = &apiOutputConfig{Format: apiFormat{Type: "json_schema", Schema: req.Schema}}
	}
	return json.Marshal(ar)
}

func (c *Client) once(ctx context.Context, body []byte) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	hr.Header.Set("x-api-key", c.opts.APIKey.Reveal())
	hr.Header.Set("anthropic-version", apiVersion)
	hr.Header.Set("content-type", "application/json")
	hr.Header.Set("user-agent", c.opts.UserAgent)

	res, err := c.http.Do(hr)
	if err != nil {
		return Response{}, &networkError{err: sanitize(err)}
	}
	defer res.Body.Close()

	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return Response{}, &networkError{err: err}
	}
	reqID := res.Header.Get("request-id")

	if res.StatusCode != http.StatusOK {
		return Response{}, parseAPIError(res, data, reqID)
	}

	var ar apiResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return Response{}, fmt.Errorf("claude: resposta inválida (request %s): %w", reqID, err)
	}
	out := Response{StopReason: ar.StopReason, Usage: ar.Usage, RequestID: reqID}

	// Com thinking ativo, a resposta pode trazer blocos "thinking" antes do texto.
	var sb strings.Builder
	for _, b := range ar.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	out.Text = sb.String()

	switch {
	case ar.StopReason == "refusal":
		return out, ErrRefusal
	case ar.StopReason == "max_tokens":
		return out, ErrTruncated
	case out.Text == "":
		return out, ErrNoText
	}
	return out, nil
}

// backoff calcula a espera: retry-after quando informado; senão, exponencial
// com jitter entre 50% e 100% do valor.
func (c *Client) backoff(attempt int, err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		return ae.RetryAfter
	}
	d := c.opts.BaseDelay << attempt
	if d <= 0 || d > c.opts.MaxDelay {
		d = c.opts.MaxDelay
	}
	return d/2 + time.Duration(c.rand()*float64(d/2))
}

// APIError é um erro HTTP devolvido pela API.
type APIError struct {
	Status     int
	Type       string
	Code       string
	Message    string
	RequestID  string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("claude: API %d %s: %s", e.Status, e.Type, e.Message)
	if e.RequestID != "" {
		s += " (request " + e.RequestID + ")"
	}
	return s
}

// SpendLimit informa se o 429 é de limite de gasto (não adianta repetir).
func (e *APIError) SpendLimit() bool { return e.Code == "enforced_spend_limit_reached" }

func parseAPIError(res *http.Response, data []byte, reqID string) *APIError {
	ae := &APIError{Status: res.StatusCode, RequestID: reqID, Type: "unknown", Message: http.StatusText(res.StatusCode)}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Details struct {
				ErrorCode string `json:"error_code"`
			} `json:"details"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(data, &body) == nil && body.Error.Type != "" {
		ae.Type, ae.Message, ae.Code = body.Error.Type, body.Error.Message, body.Error.Details.ErrorCode
		if ae.RequestID == "" {
			ae.RequestID = body.RequestID
		}
	}
	if v := res.Header.Get("retry-after"); v != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
			ae.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return ae
}

// networkError envolve falhas de transporte, que são repetíveis.
type networkError struct{ err error }

func (e *networkError) Error() string { return "claude: falha de rede: " + e.err.Error() }
func (e *networkError) Unwrap() error { return e.err }

// sanitize remove a URL do erro de transporte (por segurança e legibilidade).
func sanitize(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		return ue.Unwrap()
	}
	return err
}

func retryable(err error) bool {
	var ne *networkError
	if errors.As(err, &ne) {
		return true
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Status {
	case http.StatusTooManyRequests:
		return !ae.SpendLimit()
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}
