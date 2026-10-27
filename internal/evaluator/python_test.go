package evaluator

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"testing"
)

func TestGraderResultRejectsInvalidContract(t *testing.T) {
	for _, s := range []string{`{}`, `{"score":1.1,"passed":true,"classification":"capability_result"}`, `{"score":0.5,"passed":true,"classification":"capability_result"}`, `{"score":1,"passed":true,"classification":"unknown"}`, `{"score":1,"passed":true,"classification":"capability_result"} garbage`} {
		if _, err := ParseGraderResult([]byte(s), sandbox.ExecResult{ExitKnown: true}); err == nil {
			t.Fatalf("accepted invalid grader result %s", s)
		}
	}
}
func TestGraderResultRetainsPartialScoreAndChecks(t *testing.T) {
	r, err := ParseGraderResult([]byte(`{"score":0.3,"passed":false,"classification":"capability_result","checks":[{"name":"familyA","passed":true}],"evidence":{"policy_guard":{}}}`), sandbox.ExecResult{ExitKnown: true, ExitCode: 1})
	if err != nil || r.Score == nil || *r.Score != 0.3 || r.Verdict != "test_failure" || len(r.Checks) == 0 || len(r.GraderResult) == 0 {
		t.Fatal(r, err)
	}
}
