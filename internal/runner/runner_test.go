package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
)

var errFakeFactory = errors.New("fake factory failure")
var errFakeExec = errors.New("fake exec failure")
var errFakeDestroy = errors.New("fake destroy failure")

const fixedParser = `package parser

import (
	"errors"
	"strconv"
	"strings"
)

var ErrEmpty = errors.New("empty count")
var ErrNegative = errors.New("count must be non-negative")

func ParseCount(input string) (int, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, ErrEmpty
	}
	value, err := strconv.Atoi(input)
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, ErrNegative
	}
	return value, nil
}
`

func runnerTask() domain.Task {
	return domain.Task{
		ID: "go-parser-001", Revision: 1, Prompt: "fix ParseCount",
		SourceSHA256: "source-hash", EvaluatorSHA256: "evaluator-hash",
		Source: sandbox.Files{
			"go.mod":         []byte("module fixture/go-parser-001\n\ngo 1.27.1\n"),
			"parser.go":      []byte("package parser\n\nfunc ParseCount(input string) (int, error) { return 0, nil }\n"),
			"parser_test.go": []byte("package parser\n\nimport \"testing\"\n\nfunc TestVisible(t *testing.T) { t.Helper() }\n"),
		},
		Evaluator:     sandbox.Files{"hidden_test.go": []byte("package parser\n// trusted tests\n")},
		RequiredTests: []string{"TestEmpty", "TestNegative", "TestWhitespace", "TestRegression"},
		AllowedFiles:  []string{"parser.go"}, Budget: domain.DefaultBudget(),
		Limits: sandbox.Limits{CPUs: 1, MemoryMiB: 1024, Pids: 64, WorkspaceMiB: 64},
	}
}

type agentFunc func(context.Context, sandbox.Sandbox, domain.Task, domain.Emit) (domain.AgentResult, error)

func (f agentFunc) Run(ctx context.Context, s sandbox.Sandbox, t domain.Task, emit domain.Emit) (domain.AgentResult, error) {
	return f(ctx, s, t, emit)
}

type fakeFactory struct {
	mu             sync.Mutex
	boxes          []*fakeSandbox
	createErrAt    int
	doctorErr      error
	evalExecErr    error
	evalDestroyErr error
	agentExecErr   error
	failureOutput  string
	statsErr       error
}

func (f *fakeFactory) Doctor(context.Context) (sandbox.Info, error) {
	if f.doctorErr != nil {
		return sandbox.Info{}, f.doctorErr
	}
	return sandbox.Info{ServerVersion: "fake", OSType: "linux", Architecture: "amd64", CPUs: 4, MemoryBytes: 8 << 30, ImageID: "sha256:fixture"}, nil
}

func (f *fakeFactory) Create(_ context.Context, spec sandbox.Spec, files sandbox.Files) (sandbox.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := len(f.boxes) + 1
	if f.createErrAt == call {
		return nil, fmt.Errorf("create %d: %w", call, errFakeFactory)
	}
	role := spec.Labels["agenteval.phase"]
	b := &fakeSandbox{id: fmt.Sprintf("sandbox-%d", call), role: role, files: cloneFiles(files), initial: cloneFiles(files), factory: f}
	if role == "evaluation" {
		b.destroyErr = f.evalDestroyErr
	}
	f.boxes = append(f.boxes, b)
	return b, nil
}

func (f *fakeFactory) created() []*fakeSandbox {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeSandbox(nil), f.boxes...)
}

type fakeSandbox struct {
	mu           sync.Mutex
	id           string
	role         string
	files        sandbox.Files
	initial      sandbox.Files
	factory      *fakeFactory
	applied      bool
	patch        string
	destroyCalls int
	destroyErr   error
}

func (b *fakeSandbox) ID() string { return b.id }
func (b *fakeSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if b.role == "agent" && b.factory.agentExecErr != nil {
		return sandbox.ExecResult{}, b.factory.agentExecErr
	}
	if b.role != "evaluation" {
		return sandbox.ExecResult{ExitCode: 0, ExitKnown: true}, nil
	}
	if b.factory.evalExecErr != nil {
		return sandbox.ExecResult{}, b.factory.evalExecErr
	}
	if strings.Contains(req.Argv[0], "not-go-test") {
		return sandbox.ExecResult{}, errors.New("unexpected evaluator command")
	}
	if b.applied {
		return sandbox.ExecResult{ExitCode: 0, ExitKnown: true, Stdout: passingTestJSON()}, nil
	}
	out := b.factory.failureOutput
	if out == "" {
		out = failingTestJSON()
	}
	return sandbox.ExecResult{ExitCode: 1, ExitKnown: true, Stdout: out}, nil
}
func (b *fakeSandbox) ListFiles(context.Context, string) ([]string, error) { return nil, nil }
func (b *fakeSandbox) ReadFile(_ context.Context, name string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}
func (b *fakeSandbox) WriteFile(_ context.Context, name string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.files[name] = append([]byte(nil), data...)
	return nil
}
func (b *fakeSandbox) ApplyPatch(_ context.Context, p string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.role != "evaluation" || strings.TrimSpace(p) == "" {
		return errors.New("patch must be applied to a non-empty trusted evaluation")
	}
	b.applied = true
	b.patch = p
	b.files["parser.go"] = []byte(fixedParser)
	return nil
}
func (b *fakeSandbox) Snapshot(context.Context) (sandbox.Files, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return cloneFiles(b.files), nil
}
func (b *fakeSandbox) Stats(context.Context) (sandbox.Stats, error) {
	if b.factory.statsErr != nil {
		return sandbox.Stats{}, b.factory.statsErr
	}
	return sandbox.Stats{CPUSeconds: 0.25, MemoryBytes: 64 << 20}, nil
}
func (b *fakeSandbox) Destroy(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.destroyCalls++
	return b.destroyErr
}
func (b *fakeSandbox) destroyed() int { b.mu.Lock(); defer b.mu.Unlock(); return b.destroyCalls }

func cloneFiles(in sandbox.Files) sandbox.Files {
	out := make(sandbox.Files, len(in))
	for name, data := range in {
		out[name] = append([]byte(nil), data...)
	}
	return out
}
func passingTestJSON() string {
	var lines []string
	lines = append(lines, `{"Action":"pass","Package":"fixture/go-parser-001"}`)
	for _, name := range runnerTask().RequiredTests {
		lines = append(lines, fmt.Sprintf(`{"Action":"pass","Package":"fixture/go-parser-001","Test":%q}`, name))
	}
	return strings.Join(lines, "\n") + "\n"
}
func failingTestJSON() string {
	return strings.Join([]string{
		`{"Action":"fail","Package":"fixture/go-parser-001"}`,
		`{"Action":"fail","Package":"fixture/go-parser-001","Test":"TestEmpty"}`,
		`{"Action":"fail","Package":"fixture/go-parser-001","Test":"TestNegative"}`,
		`{"Action":"fail","Package":"fixture/go-parser-001","Test":"TestWhitespace"}`,
	}, "\n") + "\n"
}
func runFake(t *testing.T, f *fakeFactory, a domain.Agent, task domain.Task, parent context.Context) (Report, error) {
	t.Helper()
	r, err := Run(parent, f, task, a, Options{ArtifactsDir: t.TempDir(), AgentName: "scripted", AgentConfig: map[string]any{"kind": "test"}})
	return r, err
}
func readPersisted(t *testing.T, r Report) Report {
	t.Helper()
	path := filepath.Join(r.ArtifactDir, "result.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("result.json not persisted: %v", err)
	}
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode persisted report: %v", err)
	}
	if got.RunID != r.RunID || got.Verdict != r.Verdict {
		t.Fatalf("returned/persisted report mismatch: returned %+v persisted %+v", r, got)
	}
	return got
}
func assertEveryCreatedSandboxDestroyed(t *testing.T, f *fakeFactory) {
	t.Helper()
	boxes := f.created()
	for _, b := range boxes {
		if got := b.destroyed(); got < 1 {
			t.Errorf("sandbox %s Destroy calls = %d; want at least one", b.ID(), got)
		}
	}
}

func TestRunSuccessUsesFreshTrustedEvaluationSandbox(t *testing.T) {
	f := &fakeFactory{}
	a := agentFunc(func(ctx context.Context, s sandbox.Sandbox, _ domain.Task, _ domain.Emit) (domain.AgentResult, error) {
		if err := s.WriteFile(ctx, "parser.go", []byte(fixedParser)); err != nil {
			return domain.AgentResult{}, err
		}
		return domain.AgentResult{Reason: "finished", Usage: domain.Usage{Steps: 4, ToolCalls: 3}}, nil
	})
	r, err := runFake(t, f, a, runnerTask(), context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if r.Verdict != "success" || r.Status != "SUCCEEDED" {
		t.Fatalf("unexpected result: verdict=%s status=%s reason=%s", r.Verdict, r.Status, r.Reason)
	}
	boxes := f.created()
	if len(boxes) != 2 {
		t.Fatalf("created sandboxes = %d, want separate execution and evaluation sandboxes", len(boxes))
	}
	if boxes[0].ID() == boxes[1].ID() {
		t.Fatal("execution and evaluation reused the same sandbox")
	}
	if boxes[0].role != "agent" || boxes[1].role != "evaluation" {
		t.Fatalf("sandbox roles = %q, %q", boxes[0].role, boxes[1].role)
	}
	if _, exists := boxes[0].initial["hidden_test.go"]; exists {
		t.Fatal("trusted tests were visible in the agent sandbox")
	}
	if string(boxes[1].initial["parser.go"]) != string(runnerTask().Source["parser.go"]) {
		t.Fatal("evaluation did not start from original source")
	}
	if string(boxes[1].initial["parser_test.go"]) != string(runnerTask().Source["parser_test.go"]) {
		t.Fatal("evaluation did not restore the original visible tests")
	}
	if _, exists := boxes[1].initial["hidden_test.go"]; !exists {
		t.Fatal("trusted evaluator tests were not included in evaluation workspace")
	}
	if !boxes[1].applied || boxes[1].patch == "" || r.PatchSHA256 == "" {
		t.Fatal("candidate patch was not applied to trusted evaluation")
	}
	if boxes[0].destroyed() < 1 || boxes[1].destroyed() < 1 {
		t.Fatal("not every created sandbox was destroyed")
	}
	if boxes[0].destroyed() != 1 || boxes[1].destroyed() != 1 {
		t.Fatalf("successful run destroyed sandboxes %d and %d times; want one each", boxes[0].destroyed(), boxes[1].destroyed())
	}
	if persisted := readPersisted(t, r); persisted.Evaluation == nil || persisted.Evaluation.Verdict != "success" {
		t.Fatal("success evaluation was not persisted")
	}
}

func TestRunFailureReportsFailedAndMissingTrustedTests(t *testing.T) {
	f := &fakeFactory{}
	a := agentFunc(func(context.Context, sandbox.Sandbox, domain.Task, domain.Emit) (domain.AgentResult, error) {
		return domain.AgentResult{Reason: "finished"}, nil
	})
	r, err := runFake(t, f, a, runnerTask(), context.Background())
	if err != nil {
		t.Fatalf("a test-failure verdict should be persisted without infrastructure error: %v", err)
	}
	if r.Verdict != "test_failure" || r.Evaluation == nil {
		t.Fatalf("unexpected result: %+v", r)
	}
	if got, want := strings.Join(r.Evaluation.Failed, ","), "TestEmpty,TestNegative,TestWhitespace"; got != want {
		t.Errorf("failed tests = %q, want %q", got, want)
	}
	if got, want := strings.Join(r.Evaluation.Missing, ","), "TestRegression"; got != want {
		t.Errorf("missing tests = %q, want %q", got, want)
	}
	if len(r.Evaluation.Passed) != 0 {
		t.Errorf("passed tests = %v, want none", r.Evaluation.Passed)
	}
	readPersisted(t, r)
	assertEveryCreatedSandboxDestroyed(t, f)
}

func TestRunTamperedVisibleTestIsRejectedBeforeEvaluation(t *testing.T) {
	f := &fakeFactory{}
	a := agentFunc(func(ctx context.Context, s sandbox.Sandbox, _ domain.Task, _ domain.Emit) (domain.AgentResult, error) {
		return domain.AgentResult{}, s.WriteFile(ctx, "parser_test.go", []byte("package parser\nfunc TestVisible(t *testing.T) {}\n"))
	})
	r, err := runFake(t, f, a, runnerTask(), context.Background())
	if err == nil || r.Verdict != "invalid_patch" {
		t.Fatalf("tampering result = verdict %q, error %v; want invalid_patch", r.Verdict, err)
	}
	if len(f.created()) != 1 {
		t.Fatalf("evaluation sandbox created after tampering: %d sandboxes", len(f.created()))
	}
	readPersisted(t, r)
	assertEveryCreatedSandboxDestroyed(t, f)
}

func TestRunTimeoutAndCancellationPersistAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parent     context.Context
		runSeconds int
		want       string
	}{
		{name: "agent deadline", parent: context.Background(), runSeconds: 1, want: "timeout"},
		{name: "parent canceled", parent: canceledContext(), runSeconds: 600, want: "canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := runnerTask()
			task.Budget.RunSeconds = tc.runSeconds
			f := &fakeFactory{}
			a := agentFunc(func(ctx context.Context, _ sandbox.Sandbox, _ domain.Task, _ domain.Emit) (domain.AgentResult, error) {
				<-ctx.Done()
				return domain.AgentResult{}, ctx.Err()
			})
			r, err := runFake(t, f, a, task, tc.parent)
			if err == nil || r.Verdict != tc.want {
				t.Fatalf("Run() verdict=%q err=%v; want %q and error", r.Verdict, err, tc.want)
			}
			readPersisted(t, r)
			assertEveryCreatedSandboxDestroyed(t, f)
		})
	}
}
func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestRunFactoryAndExecFailuresPersistAndCleanup(t *testing.T) {
	t.Run("factory doctor", func(t *testing.T) {
		f := &fakeFactory{doctorErr: errFakeFactory}
		a := agentFunc(func(context.Context, sandbox.Sandbox, domain.Task, domain.Emit) (domain.AgentResult, error) {
			t.Fatal("agent ran after factory failure")
			return domain.AgentResult{}, nil
		})
		r, err := runFake(t, f, a, runnerTask(), context.Background())
		if err == nil || r.Verdict != "infra_error" {
			t.Fatalf("factory failure result=%q err=%v", r.Verdict, err)
		}
		readPersisted(t, r)
		if len(f.created()) != 0 {
			t.Fatal("sandbox created after doctor failure")
		}
	})
	t.Run("execution command", func(t *testing.T) {
		f := &fakeFactory{agentExecErr: errFakeExec}
		a := agentFunc(func(ctx context.Context, s sandbox.Sandbox, _ domain.Task, _ domain.Emit) (domain.AgentResult, error) {
			_, err := s.Exec(ctx, sandbox.ExecRequest{Argv: []string{"go", "test", "./..."}})
			return domain.AgentResult{}, err
		})
		r, err := runFake(t, f, a, runnerTask(), context.Background())
		if err == nil || r.Verdict != "infra_error" {
			t.Fatalf("exec failure result=%q err=%v", r.Verdict, err)
		}
		readPersisted(t, r)
		assertEveryCreatedSandboxDestroyed(t, f)
	})
	t.Run("evaluation command", func(t *testing.T) {
		f := &fakeFactory{evalExecErr: errFakeExec}
		a := agentFunc(func(ctx context.Context, s sandbox.Sandbox, _ domain.Task, _ domain.Emit) (domain.AgentResult, error) {
			return domain.AgentResult{}, s.WriteFile(ctx, "parser.go", []byte(fixedParser))
		})
		r, err := runFake(t, f, a, runnerTask(), context.Background())
		if err == nil || r.Verdict != "infra_error" {
			t.Fatalf("evaluator exec failure result=%q err=%v", r.Verdict, err)
		}
		readPersisted(t, r)
		assertEveryCreatedSandboxDestroyed(t, f)
	})
	t.Run("provider protocol malformed response", func(t *testing.T) {
		f := &fakeFactory{}
		a := agentFunc(func(context.Context, sandbox.Sandbox, domain.Task, domain.Emit) (domain.AgentResult, error) {
			return domain.AgentResult{}, domain.Fail("malformed_response", errors.New("provider tool call JSON is malformed"))
		})
		r, err := runFake(t, f, a, runnerTask(), context.Background())
		if err == nil || r.Status != "INFRA_ERROR" || r.Verdict != "infra_error" || r.Reason != "malformed_response" {
			t.Fatalf("provider protocol error result: status=%s verdict=%s reason=%s err=%v", r.Status, r.Verdict, r.Reason, err)
		}
		readPersisted(t, r)
		assertEveryCreatedSandboxDestroyed(t, f)
	})
	t.Run("evaluator sandbox create", func(t *testing.T) {
		f := &fakeFactory{createErrAt: 2}
		a := agentFunc(func(ctx context.Context, s sandbox.Sandbox, _ domain.Task, _ domain.Emit) (domain.AgentResult, error) {
			return domain.AgentResult{}, s.WriteFile(ctx, "parser.go", []byte(fixedParser))
		})
		r, err := runFake(t, f, a, runnerTask(), context.Background())
		if err == nil || r.Verdict != "infra_error" {
			t.Fatalf("eval create failure result=%q err=%v", r.Verdict, err)
		}
		readPersisted(t, r)
		assertEveryCreatedSandboxDestroyed(t, f)
	})
}

func TestRunCleanupFailureIsReportedSeparately(t *testing.T) {
	f := &fakeFactory{evalDestroyErr: errFakeDestroy}
	a := agentFunc(func(ctx context.Context, s sandbox.Sandbox, _ domain.Task, _ domain.Emit) (domain.AgentResult, error) {
		return domain.AgentResult{}, s.WriteFile(ctx, "parser.go", []byte(fixedParser))
	})
	r, err := runFake(t, f, a, runnerTask(), context.Background())
	if err == nil {
		t.Fatal("cleanup failure must be returned")
	}
	if r.CleanupStatus != "failed" || len(r.CleanupErrors) != 1 || !strings.Contains(r.CleanupErrors[0], errFakeDestroy.Error()) {
		t.Fatalf("cleanup failure not separately represented: %+v", r)
	}
	if r.Status != "INFRA_ERROR" || r.Verdict != "infra_error" || r.Reason != "cleanup_error" {
		t.Fatalf("cleanup failure should make the overall run an infrastructure error: status=%s verdict=%s reason=%s", r.Status, r.Verdict, r.Reason)
	}
	if r.Evaluation == nil || r.Evaluation.Verdict != "success" {
		t.Fatalf("cleanup failure should preserve nested successful evaluation: %+v", r.Evaluation)
	}
	persisted := readPersisted(t, r)
	if persisted.CleanupStatus != "failed" || persisted.Status != "INFRA_ERROR" || persisted.Verdict != "infra_error" || persisted.Reason != "cleanup_error" || persisted.Evaluation == nil || persisted.Evaluation.Verdict != "success" {
		t.Fatalf("persisted cleanup/vote mismatch: %+v", persisted)
	}
	assertEveryCreatedSandboxDestroyed(t, f)
}

func TestRunWriteFailureDoesNotInventZeroUsage(t *testing.T) {
	f := &fakeFactory{}
	a := agentFunc(func(ctx context.Context, s sandbox.Sandbox, _ domain.Task, _ domain.Emit) (domain.AgentResult, error) {
		if err := s.WriteFile(ctx, "parser.go", []byte(fixedParser)); err != nil {
			return domain.AgentResult{}, err
		}
		return domain.AgentResult{Usage: domain.Usage{Steps: 5, ToolCalls: 3, Quality: "known"}}, nil
	})
	r, err := runFake(t, f, a, runnerTask(), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Usage.Steps != 5 || r.Usage.ToolCalls != 3 || r.Usage.Quality != "known" {
		t.Fatalf("agent usage not preserved: %+v", r.Usage)
	}
	if r.Evaluation == nil || len(r.Evaluation.Passed) != 4 {
		t.Fatalf("trusted test counts missing or invented: %+v", r.Evaluation)
	}
	if r.Resources["agent"].Samples < 1 || r.Resources["agent"].MemoryPeakSampledBytes == nil {
		t.Fatalf("resource sampling was presented as zero instead of sampled: %+v", r.Resources["agent"])
	}
	readPersisted(t, r)
}

func TestRunResourceSamplingFailureIsMissingNotZero(t *testing.T) {
	f := &fakeFactory{statsErr: errors.New("stats unavailable")}
	a := agentFunc(func(ctx context.Context, s sandbox.Sandbox, _ domain.Task, _ domain.Emit) (domain.AgentResult, error) {
		return domain.AgentResult{}, s.WriteFile(ctx, "parser.go", []byte(fixedParser))
	})
	r, err := runFake(t, f, a, runnerTask(), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resource := r.Resources["agent"]
	if resource.Samples != 0 || resource.SampleErrors == 0 || resource.CPUSeconds != nil || resource.MemoryPeakSampledBytes != nil {
		t.Fatalf("unavailable resource samples were represented as zero: %+v", resource)
	}
	readPersisted(t, r)
}

var _ = time.Second
