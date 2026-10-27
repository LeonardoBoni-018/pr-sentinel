package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const token = "ghp_token_secreto"

var pr = PullRef{Repo: Repo{Owner: "acme", Name: "app"}, Number: 7}

type captured struct {
	method, path, query string
	header              http.Header
	body                map[string]any
}

// serve registra a última requisição e responde com handler.
func serve(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int)) (*Client, *captured, *int32, *[]time.Duration) {
	t.Helper()
	var calls int32
	cap := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(&calls, 1))
		cap.method, cap.path, cap.query, cap.header = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone()
		cap.body = nil
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &cap.body)
		}
		handler(w, r, n)
	}))
	t.Cleanup(srv.Close)
	c := New(Options{Token: token, BaseURL: srv.URL})
	var sleeps []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	return c, cap, &calls, &sleeps
}

func TestGetPullRequest(t *testing.T) {
	c, cp, _, _ := serve(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = io.WriteString(w, `{"number":7,"title":"T","body":null,"state":"open","draft":true,
			"head":{"sha":"abc"},"base":{"sha":"base1"},"user":{"login":"dependabot[bot]","type":"Bot"},"labels":[{"name":"skip-review"},{"name":"x"}]}`)
	})
	got, err := c.GetPullRequest(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadSHA != "abc" || got.BaseSHA != "base1" || got.AuthorType != "Bot" || !got.Draft || got.Title != "T" || got.Body != "" ||
		len(got.Labels) != 2 || got.Labels[0] != "skip-review" {
		t.Fatalf("got %+v", got)
	}
	if cp.method != "GET" || cp.path != "/repos/acme/app/pulls/7" {
		t.Fatalf("%s %s", cp.method, cp.path)
	}
	h := cp.header
	if h.Get("Authorization") != "Bearer "+token || h.Get("X-GitHub-Api-Version") != "2022-11-28" ||
		h.Get("Accept") != "application/vnd.github+json" || h.Get("User-Agent") == "" {
		t.Fatalf("headers = %v", h)
	}
}

func TestListFilesPaginates(t *testing.T) {
	c, _, calls, _ := serve(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		page := r.URL.Query().Get("page")
		n := 100
		if page == "2" {
			n = 3
		}
		var items []string
		for i := 0; i < n; i++ {
			items = append(items, fmt.Sprintf(`{"filename":"f%s-%d.go","status":"modified","additions":1,"deletions":2,"patch":"@@ -1 +1 @@"}`, page, i))
		}
		items[0] = strings.Replace(items[0], `,"patch":"@@ -1 +1 @@"`, "", 1) // binário: sem patch
		_, _ = io.WriteString(w, "["+strings.Join(items, ",")+"]")
	})
	files, err := c.ListFiles(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 103 || *calls != 2 {
		t.Fatalf("files=%d calls=%d", len(files), *calls)
	}
	if files[0].Patch != "" || files[1].Patch == "" || files[1].Additions != 1 || files[1].Deletions != 2 {
		t.Fatalf("files[0..1] = %+v %+v", files[0], files[1])
	}
}

func TestGetFile(t *testing.T) {
	c, cp, _, _ := serve(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if strings.HasSuffix(r.URL.Path, "missing.yml") {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		_, _ = io.WriteString(w, "language: en\n")
	})
	data, found, err := c.GetFile(context.Background(), pr.Repo, ".pr-sentinel.yml", "feature/x y")
	if err != nil || !found || string(data) != "language: en\n" {
		t.Fatalf("data=%q found=%v err=%v", data, found, err)
	}
	if cp.path != "/repos/acme/app/contents/.pr-sentinel.yml" || cp.query != "ref=feature%2Fx+y" ||
		cp.header.Get("Accept") != "application/vnd.github.raw+json" {
		t.Fatalf("path=%s query=%s accept=%s", cp.path, cp.query, cp.header.Get("Accept"))
	}

	_, found, err = c.GetFile(context.Background(), pr.Repo, "dir/missing.yml", "")
	if err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestComments(t *testing.T) {
	c, cp, _, _ := serve(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch r.Method {
		case "GET":
			_, _ = io.WriteString(w, `[{"id":5,"body":"<!-- x -->","user":{"login":"bot"}}]`)
		default:
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{}`)
		}
	})
	ctx := context.Background()
	list, err := c.ListComments(ctx, pr)
	if err != nil || len(list) != 1 || list[0].ID != 5 || list[0].Login != "bot" || cp.path != "/repos/acme/app/issues/7/comments" {
		t.Fatalf("list=%+v err=%v path=%s", list, err, cp.path)
	}
	if err := c.CreateComment(ctx, pr, "olá"); err != nil || cp.method != "POST" || cp.body["body"] != "olá" ||
		cp.path != "/repos/acme/app/issues/7/comments" {
		t.Fatalf("create: err=%v %s %s %v", err, cp.method, cp.path, cp.body)
	}
	if err := c.UpdateComment(ctx, pr.Repo, 5, "novo"); err != nil || cp.method != "PATCH" || cp.body["body"] != "novo" ||
		cp.path != "/repos/acme/app/issues/comments/5" {
		t.Fatalf("update: err=%v %s %s %v", err, cp.method, cp.path, cp.body)
	}
}

func TestReviewComments(t *testing.T) {
	c, cp, _, _ := serve(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = io.WriteString(w, `[{"path":"a.go","line":10,"body":"x"},{"path":"b.go","line":null,"body":"obsoleto"}]`)
	})
	got, err := c.ListReviewComments(context.Background(), pr)
	if err != nil || len(got) != 2 || got[0].Line != 10 || got[1].Line != 0 || cp.path != "/repos/acme/app/pulls/7/comments" {
		t.Fatalf("got=%+v err=%v path=%s", got, err, cp.path)
	}
}

func TestCreateReview(t *testing.T) {
	c, cp, _, _ := serve(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{}`)
	})
	err := c.CreateReview(context.Background(), pr, ReviewInput{
		CommitID: "abc", Body: "resumo",
		Comments: []InlineComment{{Path: "a.go", Line: 3, Body: "bug"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cp.method != "POST" || cp.path != "/repos/acme/app/pulls/7/reviews" {
		t.Fatalf("%s %s", cp.method, cp.path)
	}
	if cp.body["event"] != "COMMENT" || cp.body["commit_id"] != "abc" || cp.body["body"] != "resumo" {
		t.Fatalf("body = %v", cp.body)
	}
	cm := cp.body["comments"].([]any)[0].(map[string]any)
	if cm["path"] != "a.go" || cm["line"] != float64(3) || cm["side"] != "RIGHT" || cm["body"] != "bug" {
		t.Fatalf("comment = %v", cm)
	}
}

func TestRetryAndErrors(t *testing.T) {
	ok := func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"number":7}`) }
	tests := []struct {
		name       string
		handler    func(w http.ResponseWriter, n int)
		now        time.Time
		wantCalls  int32
		wantSleeps []time.Duration
		wantErr    string
	}{
		{
			name: "502 repete com backoff",
			handler: func(w http.ResponseWriter, n int) {
				if n < 3 {
					w.WriteHeader(502)
					return
				}
				ok(w)
			},
			wantCalls: 3, wantSleeps: []time.Duration{time.Second, 2 * time.Second},
		},
		{
			name: "limite secundário com retry-after",
			handler: func(w http.ResponseWriter, n int) {
				if n == 1 {
					w.Header().Set("retry-after", "5")
					w.WriteHeader(403)
					_, _ = io.WriteString(w, `{"message":"secondary rate limit"}`)
					return
				}
				ok(w)
			},
			wantCalls: 2, wantSleeps: []time.Duration{5 * time.Second},
		},
		{
			name: "limite primário espera o reset",
			now:  time.Unix(1000, 0),
			handler: func(w http.ResponseWriter, n int) {
				if n == 1 {
					w.Header().Set("x-ratelimit-remaining", "0")
					w.Header().Set("x-ratelimit-reset", "1010")
					w.WriteHeader(429)
					return
				}
				ok(w)
			},
			wantCalls: 2, wantSleeps: []time.Duration{11 * time.Second},
		},
		{
			name: "reset distante demais desiste",
			now:  time.Unix(1000, 0),
			handler: func(w http.ResponseWriter, _ int) {
				w.Header().Set("x-ratelimit-remaining", "0")
				w.Header().Set("x-ratelimit-reset", "999999")
				w.WriteHeader(403)
				_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
			},
			wantCalls: 1, wantErr: "rate limit exceeded",
		},
		{
			name: "403 de permissão não repete",
			handler: func(w http.ResponseWriter, _ int) {
				w.WriteHeader(403)
				_, _ = io.WriteString(w, `{"message":"Resource not accessible by integration"}`)
			},
			wantCalls: 1, wantErr: "Resource not accessible",
		},
		{
			name:      "404 não repete",
			handler:   func(w http.ResponseWriter, _ int) { w.WriteHeader(404); _, _ = io.WriteString(w, "texto puro") },
			wantCalls: 1, wantErr: "404 erro sem mensagem",
		},
		{
			name:      "esgota tentativas",
			handler:   func(w http.ResponseWriter, _ int) { w.WriteHeader(503) },
			wantCalls: 4, wantSleeps: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, wantErr: "503",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, calls, sleeps := serve(t, func(w http.ResponseWriter, _ *http.Request, n int) { tt.handler(w, n) })
			if !tt.now.IsZero() {
				c.now = func() time.Time { return tt.now }
			}
			_, err := c.GetPullRequest(context.Background(), pr)
			if (err != nil) != (tt.wantErr != "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, quer %q", err, tt.wantErr)
			}
			if *calls != tt.wantCalls {
				t.Fatalf("chamadas = %d, quer %d", *calls, tt.wantCalls)
			}
			if fmt.Sprint(*sleeps) != fmt.Sprint(tt.wantSleeps) {
				t.Fatalf("sleeps = %v, quer %v", *sleeps, tt.wantSleeps)
			}
		})
	}
}

func TestIsNotFoundAndTokenNotLeaked(t *testing.T) {
	c, _, _, _ := serve(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(404) })
	_, err := c.GetPullRequest(context.Background(), pr)
	if !IsNotFound(err) || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v", err)
	}
	if IsNotFound(nil) || IsNotFound(fmt.Errorf("x")) {
		t.Fatal("IsNotFound deveria ser falso")
	}
}

func TestNetworkErrorDoesNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := New(Options{Token: token, BaseURL: url})
	c.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := c.GetPullRequest(context.Background(), pr)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v", err)
	}
}

func TestContextCancelled(t *testing.T) {
	c, _, _, _ := serve(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(200) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetPullRequest(ctx, pr); err == nil {
		t.Fatal("esperava erro de contexto")
	}
}

func TestInvalidJSON(t *testing.T) {
	c, _, _, _ := serve(t, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = io.WriteString(w, "nope") })
	if _, err := c.GetPullRequest(context.Background(), pr); err == nil {
		t.Fatal("esperava erro")
	}
}

func TestStrings(t *testing.T) {
	if pr.String() != "acme/app#7" {
		t.Fatal(pr.String())
	}
}
