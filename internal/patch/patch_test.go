package patch

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"strings"
	"testing"
)

func TestPolicyIncludesDeletedAndNewFiles(t *testing.T) {
	base := sandbox.Files{"main.go": []byte("package fixture\n"), "main_test.go": []byte("original")}
	current := sandbox.Files{"main.go": []byte("package fixture\n// fixed\n"), "main_test.go": []byte("cheated")}
	if _, err := Generate(base, current, []string{"main.go"}); err == nil {
		t.Fatal("test tampering accepted")
	}
	delete(current, "main_test.go")
	if _, err := Generate(base, current, []string{"main.go"}); err == nil {
		t.Fatal("test deletion accepted")
	}
	current["main_test.go"] = base["main_test.go"]
	p, err := Generate(base, current, []string{"main.go"})
	if err != nil || !strings.Contains(p, "+// fixed") {
		t.Fatal(p, err)
	}
	current["new.go"] = []byte("package fixture")
	if _, err := Generate(base, current, []string{"main.go"}); err == nil {
		t.Fatal("unregistered new file accepted")
	}
}
func TestNoNewlineAndZeroChanges(t *testing.T) {
	p, err := Generate(sandbox.Files{"a.go": []byte("x")}, sandbox.Files{"a.go": []byte("y")}, []string{"a.go"})
	if err != nil || strings.Count(p, "No newline at end of file") != 2 {
		t.Fatal(p, err)
	}
	p, err = Generate(sandbox.Files{"a.go": nil}, sandbox.Files{"a.go": nil}, []string{"a.go"})
	if err != nil || p != "" {
		t.Fatal(p, err)
	}
}
