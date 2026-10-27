// Package patch derives candidate changes from the actual workspace, never agent prose.
package patch

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
	"bytes"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const MaxBytes = 1 << 20

func Generate(base, current sandbox.Files, allowed []string) (string, error) {
	allow := map[string]bool{}
	for _, n := range allowed {
		allow[n] = true
	}
	names := map[string]bool{}
	for n := range base {
		names[n] = true
	}
	for n := range current {
		names[n] = true
	}
	sorted := []string{}
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	var out strings.Builder
	for _, n := range sorted {
		a, aok := base[n]
		b, bok := current[n]
		if aok == bok && bytes.Equal(a, b) {
			continue
		}
		if !task.ValidPath(n) || !allow[n] {
			return "", fmt.Errorf("modified path not allowed: %s", n)
		}
		if !utf8.Valid(a) || !utf8.Valid(b) || bytes.IndexByte(a, 0) >= 0 || bytes.IndexByte(b, 0) >= 0 {
			return "", fmt.Errorf("binary/non-UTF8 changes unsupported")
		}
		fmt.Fprintf(&out, "diff --git a/%s b/%s\n", n, n)
		old, new := "a/"+n, "b/"+n
		if !aok {
			old = "/dev/null"
			out.WriteString("new file mode 100644\n")
		}
		if !bok {
			new = "/dev/null"
			out.WriteString("deleted file mode 100644\n")
		}
		fmt.Fprintf(&out, "--- %s\n+++ %s\n", old, new)
		al, bl := lines(a), lines(b)
		astart, bstart := 1, 1
		if len(al) == 0 {
			astart = 0
		}
		if len(bl) == 0 {
			bstart = 0
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", astart, len(al), bstart, len(bl))
		writeLines(&out, al, "-")
		writeLines(&out, bl, "+")
		if out.Len() > MaxBytes {
			return "", fmt.Errorf("candidate patch exceeds 1MiB")
		}
	}
	return out.String(), nil
}
func lines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.SplitAfter(string(b), "\n")
	if s[len(s)-1] == "" {
		s = s[:len(s)-1]
	}
	return s
}
func writeLines(b *strings.Builder, ls []string, prefix string) {
	for _, s := range ls {
		b.WriteString(prefix)
		b.WriteString(s)
		if !strings.HasSuffix(s, "\n") {
			b.WriteString("\n\\ No newline at end of file\n")
		}
	}
}
