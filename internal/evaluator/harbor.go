package evaluator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
)

func evaluateHarbor(ctx context.Context, s sandbox.Sandbox, t domain.Task) (Result, error) {
	r := Result{Verdict: "infra_error", Classification: "infrastructure_failure"}
	exec, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/sh", "/tests/test.sh"}, Workdir: t.Workdir(), Env: t.Harbor.VerifierEnv, Timeout: time.Duration(t.Budget.EvalSeconds) * time.Second, OutputLimit: 64 << 10})
	r.Exec = exec
	if err != nil {
		return r, domain.Fail("infra_error", err)
	}
	if exec.Canceled {
		return r, domain.Fail("canceled", context.Canceled)
	}
	if exec.TimedOut || exec.OOMKilled || !exec.ExitKnown || exec.ExitCode != 0 {
		return r, domain.Fail("infra_error", fmt.Errorf("Harbor verifier failed before a trusted reward was available"))
	}
	reward, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/cat", "/logs/verifier/reward.json"}, Workdir: t.Workdir(), Timeout: 5 * time.Second, OutputLimit: 4096})
	if err != nil || !reward.ExitKnown || reward.ExitCode != 0 || reward.Truncated {
		return r, domain.Fail("infra_error", fmt.Errorf("Harbor reward unavailable or oversized"))
	}
	parsed, err := ParseHarborReward([]byte(reward.Stdout), t.Harbor.PassThreshold)
	parsed.Exec = exec
	diagnostic, diagnosticErr := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/cat", "/logs/verifier/grader_report.json"}, Workdir: t.Workdir(), Timeout: 5 * time.Second, OutputLimit: 64 << 10})
	if diagnosticErr == nil && diagnostic.ExitKnown && diagnostic.ExitCode == 0 && !diagnostic.Truncated && json.Valid([]byte(diagnostic.Stdout)) {
		parsed.Diagnostics = json.RawMessage(diagnostic.Stdout)
	}
	if err != nil {
		return parsed, domain.Fail("infra_error", err)
	}
	return parsed, nil
}

// A valid zero is a model result. valid=0 is never a scored failure.
func ParseHarborReward(data []byte, threshold float64) (Result, error) {
	result := Result{Verdict: "infra_error", Classification: "infrastructure_failure"}
	var value struct {
		Reward *float64 `json:"reward"`
		Valid  *float64 `json:"valid"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if dec.Decode(&value) != nil || value.Reward == nil || value.Valid == nil {
		return result, fmt.Errorf("invalid Harbor reward contract")
	}
	if dec.Decode(new(any)) != io.EOF || math.IsNaN(*value.Reward) || math.IsInf(*value.Reward, 0) || *value.Reward < 0 || *value.Reward > 1 || (*value.Valid != 0 && *value.Valid != 1) {
		return result, fmt.Errorf("invalid Harbor reward values")
	}
	result.GraderResult = append(json.RawMessage(nil), data...)
	if *value.Valid == 0 {
		return result, fmt.Errorf("Harbor verifier reported valid=0")
	}
	if threshold <= 0 || threshold > 1 {
		return result, fmt.Errorf("invalid frozen Harbor pass threshold")
	}
	result.Score = value.Reward
	result.Classification = "capability_result"
	result.Verdict = "test_failure"
	if *value.Reward >= threshold {
		result.Verdict = "success"
		result.PackagePassed = true
	}
	return result, nil
}
