package evaluator

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"testing"
)

func TestRequiredTestsMustActuallyRun(t *testing.T) {
	output := `{"Action":"pass","Package":"fixture"}` + "\n"
	r := Parse(sandbox.ExecResult{ExitKnown: true, ExitCode: 0, Stdout: output}, []string{"TestHidden"})
	if r.Verdict == "success" || len(r.Missing) != 1 {
		t.Fatal("package pass without actual test cannot succeed", r)
	}
	output = `{"Action":"output","Package":"fixture","Output":"PASS"}` + "\n" + `{"Action":"pass","Package":"fixture","Test":"TestHidden"}` + "\n" + output
	r = Parse(sandbox.ExecResult{ExitKnown: true, ExitCode: 0, Stdout: output}, []string{"TestHidden"})
	if r.Verdict != "success" {
		t.Fatal(r)
	}
	r = Parse(sandbox.ExecResult{ExitKnown: true, ExitCode: 0, Stdout: output, Truncated: true}, []string{"TestHidden"})
	if r.Verdict == "success" {
		t.Fatal("truncated results not valid")
	}
}
