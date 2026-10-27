// Package github implementa o subconjunto da API REST do GitHub usado pelo
// pr-sentinel, além da leitura de eventos de webhook e validação de assinatura.
package github

import (
	"context"
	"fmt"

	"github.com/LeonardoBoni-018/pr-sentinel/internal/diff"
)

// Repo identifica um repositório.
type Repo struct{ Owner, Name string }

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// PullRef identifica um Pull Request.
type PullRef struct {
	Repo   Repo
	Number int
}

func (p PullRef) String() string { return fmt.Sprintf("%s#%d", p.Repo, p.Number) }

// PullRequest reúne os metadados necessários para decidir e montar a revisão.
type PullRequest struct {
	Number     int
	Title      string
	Body       string
	State      string
	Draft      bool
	HeadSHA    string
	BaseSHA    string // a config do repo é lida daqui, nunca do head do PR
	Author     string
	AuthorType string // "User", "Bot"...
	Labels     []string
}

// Comment é um comentário de issue/PR.
type Comment struct {
	ID    int64
	Body  string
	Login string
}

// ReviewComment é um comentário inline já existente no PR.
type ReviewComment struct {
	Path string
	Line int
	Body string
}

// InlineComment é um comentário inline a ser publicado (lado novo do arquivo).
type InlineComment struct {
	Path string
	Line int
	Body string
}

// ReviewInput descreve uma revisão do tipo COMMENT com comentários inline.
type ReviewInput struct {
	CommitID string
	Body     string
	Comments []InlineComment
}

// API é a superfície usada pelo núcleo; facilita mocks nos testes.
type API interface {
	GetPullRequest(ctx context.Context, pr PullRef) (PullRequest, error)
	ListFiles(ctx context.Context, pr PullRef) ([]diff.File, error)
	// GetFile devolve o conteúdo de um arquivo em ref; found=false se não existir.
	GetFile(ctx context.Context, repo Repo, path, ref string) (data []byte, found bool, err error)
	ListComments(ctx context.Context, pr PullRef) ([]Comment, error)
	CreateComment(ctx context.Context, pr PullRef, body string) error
	UpdateComment(ctx context.Context, repo Repo, id int64, body string) error
	ListReviewComments(ctx context.Context, pr PullRef) ([]ReviewComment, error)
	CreateReview(ctx context.Context, pr PullRef, in ReviewInput) error
}
