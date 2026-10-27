package diff

import (
	"fmt"
	"sort"
	"strings"
)

// Result é o diff pronto para envio, com o que ficou de fora.
type Result struct {
	Files   []File
	Omitted []string // arquivos totalmente omitidos por falta de espaço
	Partial []string // arquivos com hunks cortados
	Bytes   int
}

// Truncated informa se algo foi cortado.
func (r Result) Truncated() bool { return len(r.Omitted) > 0 || len(r.Partial) > 0 }

// Truncate encaixa os arquivos em maxBytes. Arquivos de código têm prioridade
// sobre testes, e estes sobre docs/config. O corte acontece sempre em fronteira
// de hunk, nunca no meio de uma linha. A ordem original é preservada na saída.
func Truncate(files []File, maxBytes int) Result {
	order := make([]int, len(files))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return priority(files[order[a]].Path) < priority(files[order[b]].Path)
	})

	out := make([]*File, len(files))
	var res Result
	remaining := maxBytes
	for _, idx := range order {
		f := files[idx]
		cost := size(f)
		if cost <= remaining {
			out[idx] = &f
			remaining -= cost
			continue
		}
		if cut, ok := cutHunks(f, remaining); ok {
			out[idx] = &cut
			remaining -= size(cut)
			res.Partial = append(res.Partial, f.Path)
			continue
		}
		res.Omitted = append(res.Omitted, f.Path)
	}

	for _, f := range out {
		if f != nil {
			res.Files = append(res.Files, *f)
		}
	}
	res.Bytes = maxBytes - remaining
	return res
}

func size(f File) int { return len(f.Path) + len(f.Patch) }

// cutHunks mantém os primeiros hunks inteiros que cabem em budget.
func cutHunks(f File, budget int) (File, bool) {
	hunks := splitHunks(f.Patch)
	if len(hunks) < 2 {
		return File{}, false
	}
	used := len(f.Path)
	n := 0
	for _, h := range hunks {
		if used+len(h) > budget {
			break
		}
		used += len(h)
		n++
	}
	if n == 0 || n == len(hunks) {
		return File{}, false
	}
	f.Patch = strings.Join(hunks[:n], "")
	f.Partial = true
	return f, true
}

// splitHunks divide o patch em hunks, cada um iniciando em "@@" e mantendo o
// "\n" final, de modo que strings.Join(hunks, "") reconstrói o patch.
func splitHunks(patch string) []string {
	var hunks []string
	start := 0
	for i := 0; i < len(patch); {
		nl := strings.IndexByte(patch[i:], '\n')
		next := len(patch)
		if nl >= 0 {
			next = i + nl + 1
		}
		if i > start && strings.HasPrefix(patch[i:], "@@") {
			hunks = append(hunks, patch[start:i])
			start = i
		}
		i = next
	}
	if start < len(patch) {
		hunks = append(hunks, patch[start:])
	}
	return hunks
}

// priority: 0 código, 1 testes, 2 docs/config.
func priority(p string) int {
	lower := strings.ToLower(p)
	switch {
	case isTest(lower):
		return 1
	case isDocOrConfig(lower):
		return 2
	}
	return 0
}

func isTest(p string) bool {
	for _, s := range []string{"_test.go", ".test.", ".spec.", "_test.py", "test_"} {
		if strings.Contains(p, s) {
			return true
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "test" || seg == "tests" || seg == "__tests__" || seg == "testdata" {
			return true
		}
	}
	return false
}

func isDocOrConfig(p string) bool {
	for _, ext := range []string{".md", ".txt", ".rst", ".yml", ".yaml", ".json", ".toml", ".ini", ".cfg", ".env", ".svg"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return strings.HasPrefix(p, "docs/")
}

// Format renderiza os arquivos como texto para o prompt.
func Format(files []File) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "### %s (%s, +%d -%d)\n", f.Path, f.Status, f.Additions, f.Deletions)
		b.WriteString(f.Patch)
		if !strings.HasSuffix(f.Patch, "\n") {
			b.WriteByte('\n')
		}
		if f.Partial {
			b.WriteString("[... demais hunks deste arquivo foram omitidos ...]\n")
		}
		b.WriteByte('\n')
	}
	return b.String()
}
