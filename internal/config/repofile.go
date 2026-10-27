package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// RepoFileName é o caminho do arquivo de configuração no repositório revisado.
const RepoFileName = ".pr-sentinel.yml"

// maxRepoFileBytes evita processar arquivos absurdamente grandes vindos do repo.
const maxRepoFileBytes = 64 << 10

// RepoFile representa o conteúdo de .pr-sentinel.yml. Campos ausentes
// (nil/vazios) não alteram a configuração base.
type RepoFile struct {
	Language     string   `yaml:"language"`
	MinSeverity  string   `yaml:"min_severity"`
	Ignore       []string `yaml:"ignore"`
	Instructions string   `yaml:"instructions"`
	OnError      string   `yaml:"on_error"`
}

// ParseRepoFile lê o YAML rejeitando campos desconhecidos, para que erros de
// digitação não passem despercebidos. Conteúdo vazio resulta em RepoFile zero.
func ParseRepoFile(r io.Reader) (RepoFile, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxRepoFileBytes+1))
	if err != nil {
		return RepoFile{}, fmt.Errorf("lendo %s: %w", RepoFileName, err)
	}
	if len(data) > maxRepoFileBytes {
		return RepoFile{}, fmt.Errorf("%s excede %d bytes", RepoFileName, maxRepoFileBytes)
	}
	var f RepoFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return RepoFile{}, fmt.Errorf("interpretando %s: %w", RepoFileName, err)
	}
	return f, nil
}

// Apply devolve uma cópia de c com os campos do arquivo do repositório
// aplicados por cima. Os globs de ignore são somados aos já existentes.
func (c Config) Apply(f RepoFile) Config {
	if f.Language != "" {
		c.Language = f.Language
	}
	if f.MinSeverity != "" {
		c.MinSeverity = Severity(f.MinSeverity)
	}
	if f.Instructions != "" {
		c.Instructions = f.Instructions
	}
	if f.OnError != "" {
		c.OnError = OnError(f.OnError)
	}
	if len(f.Ignore) > 0 {
		merged := make([]string, 0, len(c.Ignore)+len(f.Ignore))
		merged = append(merged, c.Ignore...)
		c.Ignore = append(merged, f.Ignore...)
	}
	return c
}
