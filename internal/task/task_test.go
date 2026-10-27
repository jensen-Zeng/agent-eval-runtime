package task

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathsAndHash(t *testing.T) {
	for _, p := range []string{"../x", "/x", "x/../y", "C:/x", "x\\y", "./x", "a\n.go", "-flag"} {
		if ValidPath(p) {
			t.Errorf("accepted %q", p)
		}
	}
	if !ValidPath("src/x.go") {
		t.Fatal("valid path")
	}
	a := HashFiles(sandbox.Files{"a": []byte("bc")})
	b := HashFiles(sandbox.Files{"ab": []byte("c")})
	if a == b {
		t.Fatal("hash ignored path boundary")
	}
	if a != HashFiles(sandbox.Files{"a": []byte("bc")}) {
		t.Fatal("unstable hash")
	}
}
func TestStrictJSONAndBudget(t *testing.T) {
	var x domain.Task
	if DecodeStrict(strings.NewReader(`{"schema_version":1,"extra":1}`), &x) == nil {
		t.Fatal("unknown field")
	}
	if DecodeStrict(strings.NewReader(`{} {}`), &x) == nil {
		t.Fatal("multiple values")
	}
	if ValidateBudget(domain.DefaultBudget()) != nil {
		t.Fatal("defaults invalid")
	}
	b := domain.DefaultBudget()
	b.RunSeconds = 0
	if ValidateBudget(b) == nil {
		t.Fatal("zero budget")
	}
	b = domain.DefaultBudget()
	b.RunSeconds = 100
	if ValidateBudget(b) == nil {
		t.Fatal("phase reserves must leave positive Agent time")
	}
}
func TestBundleLimit(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "a.go"), make([]byte, MaxFileBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBundle(d); err == nil {
		t.Fatal("large file accepted")
	}
}
