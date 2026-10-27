package diff

import (
	"reflect"
	"sort"
	"testing"
)

func TestRightLines(t *testing.T) {
	tests := []struct {
		name  string
		patch string
		want  []int
	}{
		{"vazio", "", nil},
		{"um hunk", "@@ -1,3 +1,4 @@\n ctx\n-old\n+new1\n+new2\n ctx2\n", []int{1, 2, 3, 4}},
		{"começa no meio", "@@ -10,2 +20,2 @@ func x()\n-a\n+b\n c\n", []int{20, 21}},
		{"dois hunks", "@@ -1 +1 @@\n+a\n@@ -50,2 +60,3 @@\n c\n+d\n+e\n", []int{1, 60, 61, 62}},
		{"sem newline no fim", "@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+b\n\\ No newline at end of file\n", []int{1}},
		{"arquivo novo", "@@ -0,0 +1,2 @@\n+a\n+b", []int{1, 2}},
		{"lixo antes do hunk é ignorado", "lixo\n+x\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []int
			for l := range RightLines(tt.patch) {
				got = append(got, l)
			}
			sort.Ints(got)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
