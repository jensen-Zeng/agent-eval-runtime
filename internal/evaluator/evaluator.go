package evaluator

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"strings"
	"time"
)

type Result struct {
	Diagnostics    json.RawMessage    `json:"diagnostics,omitempty"`
	Score          *float64           `json:"score,omitempty"`
	Classification string             `json:"classification,omitempty"`
	Checks         json.RawMessage    `json:"checks,omitempty"`
	GraderResult   json.RawMessage    `json:"grader_result,omitempty"`
	Verdict        string             `json:"verdict"`
	Passed         []string           `json:"passed_tests"`
	Failed         []string           `json:"failed_tests"`
	Missing        []string           `json:"missing_tests"`
	PackagePassed  bool               `json:"package_passed"`
	Exec           sandbox.ExecResult `json:"exec"`
}

func Inputs(t domain.Task) sandbox.Files {
	out := sandbox.Files{}
	for n, b := range t.Source {
		out[n] = b
	}
	for n, b := range t.Evaluator {
		if t.Grader != nil {
			out[".agenteval-evaluator/"+n] = b
		} else {
			out[n] = b
		}
	}
	return out
}

func Evaluate(ctx context.Context, s sandbox.Sandbox, t domain.Task, p string) (Result, error) {
	if t.IsHarbor() {
		return evaluateHarbor(ctx, s, t)
	}
	if t.Grader != nil {
		return evaluatePython(ctx, s, t, p)
	}
	if p != "" {
		if err := s.ApplyPatch(ctx, p); err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			return Result{Verdict: "invalid_patch"}, domain.Fail("invalid_patch", err)
		}
	}
	r, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"go", "test", "-json", "-count=1", "./..."}, Workdir: "/workspace", Timeout: time.Duration(t.Budget.EvalSeconds) * time.Second, OutputLimit: 1 << 20, Env: t.CommandEnvironment()})
	result := Parse(r, t.RequiredTests)
	if err != nil {
		return result, err
	}
	if r.TimedOut {
		return result, domain.Fail("timeout", context.DeadlineExceeded)
	}
	if r.Canceled {
		return result, domain.Fail("canceled", context.Canceled)
	}
	if r.OOMKilled {
		return result, domain.Fail("oom", fmt.Errorf("evaluator resource limit exceeded"))
	}
	if !r.ExitKnown {
		return result, domain.Fail("infra_error", fmt.Errorf("evaluator exit status unknown"))
	}
	if r.Truncated {
		return result, domain.Fail("infra_error", fmt.Errorf("evaluation output truncated; verdict invalid"))
	}
	return result, nil
}

func Parse(r sandbox.ExecResult, required []string) Result {
	x := Result{Verdict: "test_failure", Exec: r, Passed: []string{}, Failed: []string{}, Missing: []string{}}
	passed := map[string]bool{}
	failed := map[string]bool{}
	packageFail := false
	sc := bufio.NewScanner(strings.NewReader(r.Stdout))
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		var e struct {
			Action  string
			Test    string
			Package string
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Package == "" {
			continue
		}
		if e.Test != "" {
			if e.Action == "pass" {
				passed[e.Test] = true
			}
			if e.Action == "fail" {
				failed[e.Test] = true
			}
		} else {
			if e.Action == "pass" {
				x.PackagePassed = true
			}
			if e.Action == "fail" {
				packageFail = true
			}
		}
	}
	for _, n := range required {
		if failed[n] {
			x.Failed = append(x.Failed, n)
		} else if passed[n] {
			x.Passed = append(x.Passed, n)
		} else {
			x.Missing = append(x.Missing, n)
		}
	}
	if sc.Err() == nil && r.ExitKnown && r.ExitCode == 0 && !r.Truncated && !r.TimedOut && !r.Canceled && !r.OOMKilled && x.PackagePassed && !packageFail && len(x.Failed) == 0 && len(x.Missing) == 0 {
		x.Verdict = "success"
	}
	score := 0.0
	if x.Verdict == "success" {
		score = 1
	}
	if r.ExitKnown && !r.Truncated && !r.TimedOut && !r.Canceled && !r.OOMKilled {
		x.Score = &score
		x.Classification = "capability_result"
	} else {
		x.Classification = "infrastructure_failure"
	}
	return x
}
