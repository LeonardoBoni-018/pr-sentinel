package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxEventBytes limita o corpo de eventos (webhook e GITHUB_EVENT_PATH).
const maxEventBytes = 5 << 20

type prPayload struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (p prPayload) toPullRequest() PullRequest {
	pr := PullRequest{
		Number: p.Number, Title: p.Title, Body: p.Body, State: p.State, Draft: p.Draft,
		HeadSHA: p.Head.SHA, BaseSHA: p.Base.SHA, Author: p.User.Login, AuthorType: p.User.Type,
	}
	for _, l := range p.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	return pr
}

// Event é o payload de um evento pull_request, igual no webhook e no arquivo
// apontado por GITHUB_EVENT_PATH nas Actions.
type Event struct {
	Action      string    `json:"action"`
	Number      int       `json:"number"`
	PullRequest prPayload `json:"pull_request"`
	Repository  struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

// ParseEvent lê e valida o payload.
func ParseEvent(r io.Reader) (Event, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxEventBytes+1))
	if err != nil {
		return Event{}, fmt.Errorf("lendo evento: %w", err)
	}
	if len(data) > maxEventBytes {
		return Event{}, errors.New("evento excede o tamanho máximo")
	}
	return ParseEventBytes(data)
}

// ParseEventBytes é como ParseEvent, para corpos já lidos.
func ParseEventBytes(data []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		return Event{}, fmt.Errorf("evento inválido: %w", err)
	}
	if e.Number == 0 {
		e.Number = e.PullRequest.Number
	}
	if e.Number == 0 || e.Repository.Name == "" || e.Repository.Owner.Login == "" {
		return Event{}, errors.New("evento sem repositório ou número do PR (não é um evento pull_request?)")
	}
	return e, nil
}

// Ref devolve a referência do PR.
func (e Event) Ref() PullRef {
	return PullRef{Repo: Repo{Owner: e.Repository.Owner.Login, Name: e.Repository.Name}, Number: e.Number}
}

// PR devolve os metadados do PR contidos no evento.
func (e Event) PR() PullRequest { return e.PullRequest.toPullRequest() }

// Reviewable informa se a ação do evento deve disparar uma revisão.
func (e Event) Reviewable() bool {
	switch e.Action {
	case "opened", "synchronize", "reopened":
		return true
	}
	return false
}

// SignatureHeader é o header com o HMAC SHA-256 do corpo.
const SignatureHeader = "X-Hub-Signature-256"

// VerifySignature confere o header X-Hub-Signature-256 ("sha256=<hex>") contra
// o HMAC do corpo, em tempo constante. Segredo vazio sempre falha.
func VerifySignature(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Sign gera o header de assinatura (útil em testes e scripts).
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
