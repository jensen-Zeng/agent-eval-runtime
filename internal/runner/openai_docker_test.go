package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/agent"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox/docker"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
)

const (
	dockerTestGate = "AGENTEVAL_DOCKER_TESTS"
	dummyAPIKey    = "runner-test-dummy-authorization"
)

type openAIRequest struct {
	Model               string `json:"model"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
	Messages            []struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
	} `json:"messages"`
}

type scriptedToolCall struct {
	name      string
	arguments string
}

func TestDockerOpenAIProtocolEndToEnd(t *testing.T) {
	if os.Getenv(dockerTestGate) != "1" {
		t.Skipf("set %s=1 after preparing the local Docker image", dockerTestGate)
	}

	projectRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(projectRoot, "tasks", "fixtures", "go-parser-001", "task.json")
	loaded, err := task.Load(manifest, projectRoot)
	if err != nil {
		t.Fatalf("load trusted fixture: %v", err)
	}
	// CI may compile the Go standard library cold while image builds compete for CPU.
	// Keep this integration allowance bounded without measuring compile time as a runtime SLA.
	loaded.Budget.CommandSeconds = 120
	writeArgs := successWriteFileArguments(t, loaded)
	calls := []scriptedToolCall{
		{name: "read_file", arguments: `{"path":"parser.go"}`},
		{name: "write_file", arguments: writeArgs},
		{name: "exec", arguments: `{"argv":["go","test","./..."],"workdir":null,"timeout_seconds":120}`},
		{name: "finish", arguments: `{"summary":"Implemented the parser fix and ran the visible tests."}`},
	}

	var mu sync.Mutex
	requestCount := 0
	badAuthorization := false
	unexpectedModel := false
	wrongPhase := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		mu.Lock()
		callIndex := requestCount
		requestCount++
		mu.Unlock()

		if got := r.Header.Get("Authorization"); got != "Bearer "+dummyAPIKey {
			mu.Lock()
			badAuthorization = true
			mu.Unlock()
		}
		var request openAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request JSON", http.StatusBadRequest)
			return
		}
		if request.Model != "fixture-model" || request.MaxCompletionTokens <= 0 {
			mu.Lock()
			unexpectedModel = true
			mu.Unlock()
		}
		phase := 0
		for _, message := range request.Messages {
			if message.Role == "tool" {
				phase++
			}
		}
		if phase != callIndex {
			mu.Lock()
			wrongPhase = true
			mu.Unlock()
		}
		if phase >= len(calls) {
			http.Error(w, "unexpected extra model request", http.StatusBadRequest)
			return
		}
		call := calls[phase]
		response := map[string]any{
			"id": "chatcmpl-local-fixture",
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id":       fmt.Sprintf("call_%d", callIndex+1),
						"type":     "function",
						"function": map[string]string{"name": call.name, "arguments": call.arguments},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]int{"prompt_tokens": 160, "completion_tokens": 24, "total_tokens": 184},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	factory, err := docker.New(ctx, docker.Options{Image: "agenteval-sandbox:v1.0"})
	if err != nil {
		t.Fatalf("connect to Docker Engine: %v", err)
	}
	defer factory.Close()

	artifactsDir := t.TempDir()
	selectedAgent := agent.LLM{Config: agent.Config{
		BaseURL:             server.URL,
		Model:               "fixture-model",
		APIKey:              dummyAPIKey,
		HTTPClient:          server.Client(),
		MaxCompletionTokens: 512,
		MaxRetries:          0,
	}}
	report, err := Run(ctx, factory, loaded, selectedAgent, Options{
		ArtifactsDir: artifactsDir,
		AgentName:    "openai-compatible-http-fixture",
		AgentConfig:  map[string]any{"model": "fixture-model", "base_url": server.URL},
	})
	if err != nil {
		t.Fatalf("run Docker plus mocked provider: %v\n%s", err, dockerReportDiagnostics(report))
	}
	if report.Status != "SUCCEEDED" || report.Verdict != "success" {
		t.Fatalf("unexpected run result: status=%s verdict=%s reason=%s\n%s", report.Status, report.Verdict, report.Reason, dockerReportDiagnostics(report))
	}
	if report.Evaluation == nil || report.Evaluation.Verdict != "success" {
		t.Fatalf("trusted evaluation did not pass\n%s", dockerReportDiagnostics(report))
	}
	if len(report.Evaluation.Passed) != len(loaded.RequiredTests) {
		t.Fatalf("trusted evaluation passed %d registered tests, want %d: %v", len(report.Evaluation.Passed), len(loaded.RequiredTests), report.Evaluation.Passed)
	}
	passed := make(map[string]bool, len(report.Evaluation.Passed))
	for _, name := range report.Evaluation.Passed {
		passed[name] = true
	}
	for _, name := range loaded.RequiredTests {
		if !passed[name] {
			t.Errorf("registered trusted test %s was not reported as passed", name)
		}
	}
	if len(report.SandboxIDs) != 2 || report.SandboxIDs[0] == report.SandboxIDs[1] {
		t.Fatalf("execution and evaluator should use two independent sandbox IDs: %v", report.SandboxIDs)
	}
	if report.CleanupStatus != "succeeded" {
		t.Fatalf("sandbox cleanup status = %q, errors=%v", report.CleanupStatus, report.CleanupErrors)
	}
	if report.Usage.ModelCalls != len(calls) || report.Usage.ToolCalls != len(calls) || report.Usage.Steps != len(calls) {
		t.Errorf("actual provider/tool usage not recorded: %+v", report.Usage)
	}
	if report.Usage.InputTokens == 0 || report.Usage.OutputTokens == 0 || report.Usage.TotalTokens == 0 {
		t.Errorf("mock-reported token usage was not recorded: %+v", report.Usage)
	}
	if report.ArtifactDir == "" {
		t.Fatal("runner omitted artifact directory")
	}
	if _, err := os.Stat(filepath.Join(report.ArtifactDir, "result.json")); err != nil {
		t.Fatalf("run report was not persisted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(report.ArtifactDir, "attempts", report.AttemptID, "evaluation.json")); err != nil {
		t.Fatalf("trusted evaluation artifact was not persisted: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if badAuthorization {
		t.Error("mock provider did not receive the dummy Authorization header")
	}
	if unexpectedModel {
		t.Error("mock provider request used an unexpected model or completion budget")
	}
	if wrongPhase {
		t.Error("mock provider response sequence did not follow returned tool results in request.messages")
	}
	if requestCount != len(calls) {
		t.Errorf("mock provider requests = %d, want %d", requestCount, len(calls))
	}
}

func dockerReportDiagnostics(report Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run: status=%s verdict=%s reason=%s run_id=%s cleanup=%s cleanup_errors=%v\n", report.Status, report.Verdict, report.Reason, report.RunID, report.CleanupStatus, report.CleanupErrors)
	if report.Evaluation != nil {
		e := report.Evaluation
		fmt.Fprintf(&b, "evaluation: verdict=%s package_passed=%t passed=%v failed=%v missing=%v exit_code=%d exit_known=%t timed_out=%t oom_killed=%t\n", e.Verdict, e.PackagePassed, e.Passed, e.Failed, e.Missing, e.Exec.ExitCode, e.Exec.ExitKnown, e.Exec.TimedOut, e.Exec.OOMKilled)
		fmt.Fprintf(&b, "evaluation stdout:\n%s\nevaluation stderr:\n%s\n", e.Exec.Stdout, e.Exec.Stderr)
	} else {
		b.WriteString("evaluation: no result attached to the run report\n")
	}
	if report.ArtifactDir == "" {
		return b.String()
	}
	patchPath := filepath.Join(report.ArtifactDir, "attempts", report.AttemptID, "candidate.patch")
	if patch, err := os.ReadFile(patchPath); err == nil {
		fmt.Fprintf(&b, "candidate patch:\n%s\n", patch)
	}
	tracePath := filepath.Join(report.ArtifactDir, "attempts", report.AttemptID, "trace.jsonl")
	if trace, err := os.ReadFile(tracePath); err == nil {
		lines := strings.Split(strings.TrimSpace(string(trace)), "\n")
		if len(lines) > 12 {
			lines = lines[len(lines)-12:]
		}
		fmt.Fprintf(&b, "last trace events:\n%s\n", strings.Join(lines, "\n"))
	}
	return b.String()
}

func successWriteFileArguments(t *testing.T, loaded domain.Task) string {
	t.Helper()
	for _, action := range loaded.Scripts["success"].Actions {
		if action.Tool != "write_file" {
			continue
		}
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(action.Arguments, &args); err != nil {
			t.Fatalf("decode fixture's successful write_file action: %v", err)
		}
		if args.Path != "parser.go" || strings.TrimSpace(args.Content) == "" {
			t.Fatalf("fixture success action does not contain the expected parser.go patch: %+v", args)
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("encode write_file tool arguments: %v", err)
		}
		return string(encoded)
	}
	t.Fatal("fixture success action has no write_file step")
	return ""
}
