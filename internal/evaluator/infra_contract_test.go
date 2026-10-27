package evaluator

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"testing"
)

func TestInfrastructureGraderNullScoreRetainsEvidence(t *testing.T) {
	b := []byte(`{"score":null,"passed":false,"classification":"infrastructure_failure","evidence":{"missing":"module-cache"}}`)
	r, err := ParseGraderResult(b, sandbox.ExecResult{ExitKnown: true, ExitCode: 2})
	if err != nil || r.Verdict != "infra_error" || r.Score != nil || len(r.GraderResult) == 0 {
		t.Fatal(r, err)
	}
	if _, err := ParseGraderResult(b, sandbox.ExecResult{ExitKnown: true, ExitCode: 0}); err == nil {
		t.Fatal("inconsistent successful exit accepted")
	}
}
