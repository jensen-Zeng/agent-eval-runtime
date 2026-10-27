package evaluator

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Graders are trusted operator inputs. They execute only in a fresh evaluation
// sandbox; neither their source nor an oracle is sent to an Agent.
func evaluatePython(ctx context.Context, s sandbox.Sandbox, t domain.Task, p string) (Result, error) {
	timeout := time.Duration(t.Budget.EvalSeconds) * time.Second
	result := Result{Verdict: "infra_error", Classification: "infrastructure_failure"}
	env := append([]string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=/workspace", "GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=/dev/null"}, t.CommandEnvironment()...)
	// Imported source has no Git metadata. Establish a clean reviewed baseline so
	// the original grader's production-only policy checks remain meaningful.
	init, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"sh", "-c", "git -c core.hooksPath=/dev/null init -q && printf '.agenteval-evaluator/\n' > .git/info/exclude && git -c core.hooksPath=/dev/null add -- . && git -c core.hooksPath=/dev/null -c user.name=AgentEval -c user.email=runtime@localhost commit -qm baseline"}, Workdir: "/workspace", Env: env, Timeout: 30 * time.Second})
	result.Exec = init
	if err != nil || !init.ExitKnown || init.ExitCode != 0 {
		return result, domain.Fail("infra_error", fmt.Errorf("grader baseline initialization failed"))
	}
	if p != "" {
		if err := s.ApplyPatch(ctx, p); err != nil {
			return Result{Verdict: "invalid_patch", Classification: "invalid_submission"}, domain.Fail("invalid_patch", err)
		}
	}
	output := ".agenteval-evaluator/result.json"
	argv := []string{"python3", ".agenteval-evaluator/" + t.Grader.EntryPoint, "/workspace", "--go", "/usr/local/go/bin/go", "--output", output}
	if cache := t.Environment.GoModuleCache; cache != "" {
		argv = append(argv, "--module-cache", cache)
	}
	exec, err := s.Exec(ctx, sandbox.ExecRequest{Argv: argv, Workdir: "/workspace", Timeout: timeout, Env: env, OutputLimit: 64 << 10})
	result.Exec = exec
	if err != nil {
		return result, err
	}
	if exec.TimedOut {
		return result, domain.Fail("timeout", context.DeadlineExceeded)
	}
	if exec.Canceled {
		return result, domain.Fail("canceled", context.Canceled)
	}
	if exec.OOMKilled {
		return result, domain.Fail("oom", fmt.Errorf("grader resource limit"))
	}
	if !exec.ExitKnown {
		return result, domain.Fail("infra_error", fmt.Errorf("grader exit unknown"))
	}
	// Detect persistent corruption of the trusted grader inputs.
	for n, b := range t.Evaluator {
		actual, e := s.ReadFile(ctx, ".agenteval-evaluator/"+n)
		if e != nil || !bytes.Equal(actual, b) {
			return result, domain.Fail("infra_error", fmt.Errorf("trusted grader integrity check failed"))
		}
	}
	data, e := s.ReadFile(ctx, output)
	if e != nil || len(data) > 512<<10 {
		return result, domain.Fail("infra_error", fmt.Errorf("grader result unavailable or oversized"))
	}
	parsed, e := ParseGraderResult(data, exec)
	if e != nil {
		return result, domain.Fail("infra_error", e)
	}
	return parsed, nil
}
func ParseGraderResult(data []byte, exec sandbox.ExecResult) (Result, error) {
	var p struct {
		Score          *float64        `json:"score"`
		Passed         *bool           `json:"passed"`
		Classification string          `json:"classification"`
		Checks         json.RawMessage `json:"checks"`
	}
	if !json.Valid(data) || json.Unmarshal(data, &p) != nil || p.Passed == nil || !exec.ExitKnown {
		return Result{}, fmt.Errorf("invalid grader result contract")
	}
	if p.Classification == "infrastructure_failure" {
		if *p.Passed || exec.ExitCode != 2 || (p.Score != nil && (math.IsNaN(*p.Score) || *p.Score < 0 || *p.Score > 1)) {
			return Result{}, fmt.Errorf("inconsistent infrastructure failure")
		}
		return Result{Verdict: "infra_error", Score: nil, Classification: p.Classification, Checks: p.Checks, GraderResult: append(json.RawMessage(nil), data...), Exec: exec}, nil
	}
	if p.Score == nil || math.IsNaN(*p.Score) || *p.Score < 0 || *p.Score > 1 {
		return Result{}, fmt.Errorf("invalid grader result contract")
	}
	r := Result{Verdict: "test_failure", Score: p.Score, Classification: p.Classification, Checks: p.Checks, GraderResult: append(json.RawMessage(nil), data...), Exec: exec}
	switch p.Classification {
	case "capability_result":
		if *p.Passed && *p.Score == 1 && exec.ExitCode == 0 {
			r.Verdict = "success"
		} else if *p.Passed || (*p.Score == 1 && exec.ExitCode == 0) {
			return Result{}, fmt.Errorf("inconsistent grader success")
		}
	case "invalid_submission":
		if *p.Passed || exec.ExitCode != 1 {
			return Result{}, fmt.Errorf("inconsistent invalid submission")
		}
		r.Verdict = "invalid_patch"
	case "infrastructure_failure":
		r.Verdict = "infra_error"
	default:
		return Result{}, fmt.Errorf("unknown grader classification")
	}
	return r, nil
}
