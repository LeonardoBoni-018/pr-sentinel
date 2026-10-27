package diff

import (
	"regexp"
	"strconv"
	"strings"
)

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// RightLines devolve os números de linha (lado novo do arquivo) que aparecem
// no patch, ou seja, as únicas onde a API do GitHub aceita comentários inline
// com side=RIGHT.
func RightLines(patch string) map[int]bool {
	lines := map[int]bool{}
	next := 0
	for _, l := range strings.Split(patch, "\n") {
		if m := hunkHeader.FindStringSubmatch(l); m != nil {
			next, _ = strconv.Atoi(m[1])
			continue
		}
		if next == 0 || l == "" {
			continue
		}
		switch l[0] {
		case '+', ' ':
			lines[next] = true
			next++
		}
		// '-' não existe no lado novo; '\' ("No newline at end of file") é ignorada.
	}
	return lines
}
