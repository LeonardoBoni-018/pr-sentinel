package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/config"
	"github.com/LeonardoBoni-018/pr-sentinel/internal/diff"
)

const (
	// DefaultBaseURL é a API pública do GitHub.
	DefaultBaseURL = "https://api.github.com"
	apiVersion     = "2022-11-28"

	perPage          = 100
	maxPages         = 30 // a API limita a 3000 arquivos por PR
	maxResponseBytes = 16 << 20
	maxWait          = 60 * time.Second
	maxRetries       = 3
)

// Options configura o Client.
type Options struct {
	Token      config.Secret
	BaseURL    string
	UserAgent  string
	Timeout    time.Duration
	HTTPClient *http.Client
}

// Client implementa API sobre net/http.
type Client struct {
	token   config.Secret
	baseURL string
	ua      string
	http    *http.Client
	sleep   func(context.Context, time.Duration) error
	now     func() time.Time
}

var _ API = (*Client)(nil)

// New cria um Client.
func New(o Options) *Client {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	if o.UserAgent == "" {
		o.UserAgent = "pr-sentinel"
	}
	h := o.HTTPClient
	if h == nil {
		to := o.Timeout
		if to <= 0 {
			to = 30 * time.Second
		}
		h = &http.Client{Timeout: to}
	}
	return &Client{
		token: o.Token, baseURL: strings.TrimRight(o.BaseURL, "/"), ua: o.UserAgent,
		http: h, sleep: sleepCtx, now: time.Now,
	}
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

// APIError é uma resposta de erro da API do GitHub.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound informa se err é um 404.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

type request struct {
	method string
	path   string // já com query
	body   any
	accept string
}

// do executa a requisição com retry em 5xx e rate limits, devolvendo o corpo.
func (c *Client) do(ctx context.Context, rq request) ([]byte, error) {
	var payload []byte
	if rq.body != nil {
		var err error
		if payload, err = json.Marshal(rq.body); err != nil {
			return nil, err
		}
	}
	accept := rq.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		hr, err := http.NewRequestWithContext(ctx, rq.method, c.baseURL+rq.path, rdr)
		if err != nil {
			return nil, err
		}
		hr.Header.Set("Authorization", "Bearer "+c.token.Reveal())
		hr.Header.Set("Accept", accept)
		hr.Header.Set("X-GitHub-Api-Version", apiVersion)
		hr.Header.Set("User-Agent", c.ua)
		if payload != nil {
			hr.Header.Set("Content-Type", "application/json")
		}

		res, err := c.http.Do(hr)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("github: %s %s: falha de rede", rq.method, rq.path)
			if attempt < maxRetries {
				if err := c.sleep(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
					return nil, err
				}
				continue
			}
			return nil, lastErr
		}
		data, rerr := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
		_ = res.Body.Close()
		if rerr != nil {
			return nil, fmt.Errorf("github: lendo resposta de %s %s: %w", rq.method, rq.path, rerr)
		}
		if res.StatusCode < 300 {
			return data, nil
		}

		lastErr = &APIError{Method: rq.method, Path: rq.path, Status: res.StatusCode, Message: errorMessage(data)}
		wait, ok := c.retryWait(res, attempt)
		if !ok || attempt == maxRetries {
			return nil, lastErr
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func errorMessage(data []byte) string {
	var b struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &b) == nil && b.Message != "" {
		return b.Message
	}
	return "erro sem mensagem"
}

// retryWait decide se a resposta é repetível e por quanto tempo esperar.
// Segue a documentação: retry-after quando presente; x-ratelimit-reset quando
// o limite primário zerou; 5xx de gateway com backoff exponencial.
func (c *Client) retryWait(res *http.Response, attempt int) (time.Duration, bool) {
	switch res.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return time.Duration(1<<attempt) * time.Second, true
	case http.StatusForbidden, http.StatusTooManyRequests:
		if v := res.Header.Get("retry-after"); v != "" {
			if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s >= 0 {
				return clampWait(time.Duration(s) * time.Second)
			}
		}
		if res.Header.Get("x-ratelimit-remaining") == "0" {
			if s, err := strconv.ParseInt(res.Header.Get("x-ratelimit-reset"), 10, 64); err == nil {
				return clampWait(time.Unix(s, 0).Sub(c.now()) + time.Second)
			}
		}
	}
	return 0, false
}

// clampWait recusa esperas além de maxWait (melhor falhar do que travar a Action).
func clampWait(d time.Duration) (time.Duration, bool) {
	if d < 0 {
		d = 0
	}
	return d, d <= maxWait
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	data, err := c.do(ctx, request{method: http.MethodGet, path: path})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github: resposta inválida de GET %s: %w", path, err)
	}
	return nil
}

// paginate percorre páginas de 100 itens até esgotar ou atingir maxPages.
func paginate[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var all []T
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= maxPages; page++ {
		var items []T
		if err := c.getJSON(ctx, fmt.Sprintf("%s%sper_page=%d&page=%d", path, sep, perPage, page), &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < perPage {
			break
		}
	}
	return all, nil
}

func repoPath(r Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

func pullPath(p PullRef) string { return repoPath(p.Repo) + "/pulls/" + strconv.Itoa(p.Number) }

// GetPullRequest busca os metadados do PR.
func (c *Client) GetPullRequest(ctx context.Context, pr PullRef) (PullRequest, error) {
	var raw prPayload
	if err := c.getJSON(ctx, pullPath(pr), &raw); err != nil {
		return PullRequest{}, err
	}
	return raw.toPullRequest(), nil
}

// ListFiles lista os arquivos alterados já como diff.File.
func (c *Client) ListFiles(ctx context.Context, pr PullRef) ([]diff.File, error) {
	type apiFile struct {
		Filename  string `json:"filename"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
		Patch     string `json:"patch"`
	}
	items, err := paginate[apiFile](ctx, c, pullPath(pr)+"/files")
	if err != nil {
		return nil, err
	}
	files := make([]diff.File, 0, len(items))
	for _, f := range items {
		files = append(files, diff.File{Path: f.Filename, Status: f.Status, Patch: f.Patch, Additions: f.Additions, Deletions: f.Deletions})
	}
	return files, nil
}

// GetFile lê o conteúdo bruto de um arquivo do repositório em ref.
func (c *Client) GetFile(ctx context.Context, repo Repo, path, ref string) ([]byte, bool, error) {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	p := repoPath(repo) + "/contents/" + strings.Join(segs, "/")
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	data, err := c.do(ctx, request{method: http.MethodGet, path: p, accept: "application/vnd.github.raw+json"})
	if IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// ListComments lista os comentários de conversa do PR.
func (c *Client) ListComments(ctx context.Context, pr PullRef) ([]Comment, error) {
	type apiComment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	items, err := paginate[apiComment](ctx, c, repoPath(pr.Repo)+"/issues/"+strconv.Itoa(pr.Number)+"/comments")
	if err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(items))
	for _, i := range items {
		out = append(out, Comment{ID: i.ID, Body: i.Body, Login: i.User.Login})
	}
	return out, nil
}

// CreateComment publica um comentário de conversa.
func (c *Client) CreateComment(ctx context.Context, pr PullRef, body string) error {
	_, err := c.do(ctx, request{
		method: http.MethodPost,
		path:   repoPath(pr.Repo) + "/issues/" + strconv.Itoa(pr.Number) + "/comments",
		body:   map[string]string{"body": body},
	})
	return err
}

// UpdateComment edita um comentário de conversa existente.
func (c *Client) UpdateComment(ctx context.Context, repo Repo, id int64, body string) error {
	_, err := c.do(ctx, request{
		method: http.MethodPatch,
		path:   repoPath(repo) + "/issues/comments/" + strconv.FormatInt(id, 10),
		body:   map[string]string{"body": body},
	})
	return err
}

// ListReviewComments lista os comentários inline do PR (para evitar duplicatas).
func (c *Client) ListReviewComments(ctx context.Context, pr PullRef) ([]ReviewComment, error) {
	type apiRC struct {
		Path string `json:"path"`
		Line *int   `json:"line"`
		Body string `json:"body"`
	}
	items, err := paginate[apiRC](ctx, c, pullPath(pr)+"/comments")
	if err != nil {
		return nil, err
	}
	out := make([]ReviewComment, 0, len(items))
	for _, i := range items {
		rc := ReviewComment{Path: i.Path, Body: i.Body}
		if i.Line != nil {
			rc.Line = *i.Line
		}
		out = append(out, rc)
	}
	return out, nil
}

// CreateReview publica uma revisão do tipo COMMENT com comentários inline.
func (c *Client) CreateReview(ctx context.Context, pr PullRef, in ReviewInput) error {
	type apiComment struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Side string `json:"side"`
		Body string `json:"body"`
	}
	body := struct {
		CommitID string       `json:"commit_id,omitempty"`
		Body     string       `json:"body"`
		Event    string       `json:"event"`
		Comments []apiComment `json:"comments,omitempty"`
	}{CommitID: in.CommitID, Body: in.Body, Event: "COMMENT"}
	for _, ic := range in.Comments {
		body.Comments = append(body.Comments, apiComment{Path: ic.Path, Line: ic.Line, Side: "RIGHT", Body: ic.Body})
	}
	_, err := c.do(ctx, request{method: http.MethodPost, path: pullPath(pr) + "/reviews", body: body})
	return err
}
