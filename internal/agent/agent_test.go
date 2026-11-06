package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/tools"
)

type fakeBox struct {
	files         map[string][]byte
	last          sandbox.ExecRequest
	calls         int
	execResult    sandbox.ExecResult
	execErr       error
	useExecResult bool
}

func (f *fakeBox) ID() string { return "fake" }
func (f *fakeBox) Exec(ctx context.Context, r sandbox.ExecRequest) (sandbox.ExecResult, error) {
	f.last = r
	if f.useExecResult {
		return f.execResult, f.execErr
	}
	return sandbox.ExecResult{ExitCode: 0, ExitKnown: true, Stdout: "ok"}, nil
}
func (f *fakeBox) ListFiles(_ context.Context, p string) ([]string, error) {
	f.calls++
	out := []string{}
	for k := range f.files {
		if p == "" || strings.HasPrefix(k, p+"/") {
			out = append(out, k)
		}
	}
	return out, nil
}
func (f *fakeBox) ReadFile(_ context.Context, p string) ([]byte, error) {
	b, ok := f.files[p]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), b...), nil
}
func (f *fakeBox) WriteFile(_ context.Context, p string, b []byte) error {
	f.files[p] = append([]byte(nil), b...)
	return nil
}
func (f *fakeBox) ApplyPatch(context.Context, string) error        { return nil }
func (f *fakeBox) Snapshot(context.Context) (sandbox.Files, error) { return f.files, nil }
func (f *fakeBox) Stats(context.Context) (sandbox.Stats, error)    { return sandbox.Stats{}, nil }
func (f *fakeBox) Destroy(context.Context) error                   { return nil }

func TestLLMToolLoopFinish(t *testing.T) {
	box := &fakeBox{files: map[string][]byte{"main.go": []byte("before")}}
	var gotToolMessage bool
	var authorization string
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		authorization = r.Header.Get("Authorization")
		var req struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			t.Error(e)
			return
		}
		if requests == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"main.go\",\"content\":\"after\"}"}}]}}]}`))
			return
		}
		for _, m := range req.Messages {
			if string(m["role"]) == `"tool"` {
				gotToolMessage = true
				if string(m["tool_call_id"]) != `"call-1"` {
					t.Errorf("wrong tool call id: %s", m["tool_call_id"])
				}
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-2","type":"function","function":{"name":"finish","arguments":"{\"summary\":\"done\"}"}}]}}]}`))
	}))
	defer srv.Close()
	var events []string
	var trace string
	emit := func(_, event string, data any) {
		events = append(events, event)
		b, _ := json.Marshal(data)
		trace += string(b)
	}
	res, err := (LLM{Config: Config{BaseURL: srv.URL, Model: "test-model", APIKey: "secret-test-key"}}).Run(context.Background(), box, domain.Task{Prompt: "Edit the source", Budget: domain.DefaultBudget()}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != "completed" || res.Summary != "done" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if string(box.files["main.go"]) != "after" {
		t.Fatalf("tool did not edit workspace")
	}
	if requests != 2 || !gotToolMessage {
		t.Fatalf("tool protocol not followed: requests=%d toolMessage=%t", requests, gotToolMessage)
	}
	if authorization != "Bearer secret-test-key" {
		t.Fatal("authorization missing")
	}
	if res.Usage.ModelCalls != 2 || res.Usage.Steps != 2 {
		t.Fatalf("usage not tracked: %+v", res.Usage)
	}
	for _, ev := range events {
		if ev == "model_content" {
			t.Fatal("raw model content emitted")
		}
	}
	if strings.Contains(trace, "secret-test-key") || !strings.Contains(trace, "[redacted]") || !strings.Contains(trace, `"result":`) {
		t.Fatalf("trace should be useful and redact secret fields: %s", trace)
	}
}

func TestLLMReadImageReturnsImageURLAfterToolResult(t *testing.T) {
	var imageBytes bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	if err := png.Encode(&imageBytes, img); err != nil {
		t.Fatal(err)
	}
	box := &fakeBox{files: map[string][]byte{"assets/input.png": imageBytes.Bytes()}}
	requests := 0
	var secondMessages []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req struct {
			Messages []map[string]any `json:"messages"`
			Tools    []map[string]any `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if requests == 1 {
			toolNames, _ := json.Marshal(req.Tools)
			if !strings.Contains(string(toolNames), "read_image") {
				t.Errorf("Harbor request is missing read_image definition: %s", toolNames)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"image-call","type":"function","function":{"name":"read_image","arguments":"{\"path\":\"assets/input.png\",\"frame\":null}"}}]}}]}`))
			return
		}
		secondMessages = req.Messages
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"finish-call","type":"function","function":{"name":"finish","arguments":"{\"summary\":\"image inspected\"}"}}]}}]}`))
	}))
	defer srv.Close()
	var trace string
	emit := func(_, _ string, data any) { b, _ := json.Marshal(data); trace += string(b) }
	task := domain.Task{Backend: "harbor", Harbor: &domain.HarborSpec{Workdir: "/app"}, Prompt: "Inspect this image", Budget: domain.DefaultBudget()}
	result, err := (LLM{Config: Config{BaseURL: srv.URL, Model: "test-model", APIKey: "secret"}}).Run(context.Background(), box, task, emit)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "image inspected" || requests != 2 {
		t.Fatalf("image tool loop did not finish: result=%+v requests=%d", result, requests)
	}
	toolIndex, imageIndex := -1, -1
	for i, message := range secondMessages {
		if message["role"] == "tool" && message["tool_call_id"] == "image-call" {
			toolIndex = i
		}
		if message["role"] == "user" {
			parts, ok := message["content"].([]any)
			if !ok {
				continue
			}
			for _, part := range parts {
				partMap, _ := part.(map[string]any)
				imageURL, _ := partMap["image_url"].(map[string]any)
				url, _ := imageURL["url"].(string)
				if strings.HasPrefix(url, "data:image/png;base64,") {
					imageIndex = i
				}
			}
		}
	}
	if toolIndex < 0 || imageIndex <= toolIndex {
		t.Fatalf("tool result and following image_url part missing or misordered: %#v", secondMessages)
	}
	if strings.Contains(trace, "data:image") || strings.Contains(trace, "secret") {
		t.Fatalf("trace must exclude image payload and credentials: %s", trace)
	}
	if got, want := estimateRequestTokens([]map[string]any{{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("a", 100)}}}}}, nil), estimateRequestTokens([]map[string]any{{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("a", 100000)}}}}}, nil); got != want {
		t.Fatalf("image token estimate must use fixed budget, got small=%d large=%d", got, want)
	}
}

func TestRouterStrictPathsLimitsAndCommandTimeout(t *testing.T) {
	box := &fakeBox{files: map[string][]byte{}}
	r := toolsRouter(box, domain.Task{Budget: domain.Budget{CommandSeconds: 4}, AllowedFiles: []string{"ok.txt"}})
	for _, args := range []string{`{"path":"../x","content":"x"}`, `{"path":"C:/x","content":"x"}`, `{"path":"ok.txt","content":"x","env":{"SECRET":"x"}}`} {
		if _, e := r.Call(context.Background(), "write_file", json.RawMessage(args)); e == nil {
			t.Fatalf("accepted unsafe args %s", args)
		}
	}
	if _, e := r.Call(context.Background(), "write_file", json.RawMessage(`{"path":"ok.txt","content":"pass"}`)); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Call(context.Background(), "exec", json.RawMessage(`{"argv":["go","test","./..."],"workdir":".","timeout_seconds":9}`)); e != nil {
		t.Fatal(e)
	}
	if box.last.Timeout != 4*time.Second {
		t.Fatalf("command timeout = %s; want 4s", box.last.Timeout)
	}
	if box.last.OutputLimit != 64<<10 {
		t.Fatalf("command output limit = %d; want %d", box.last.OutputLimit, 64<<10)
	}
	wantEnv := []string{"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off"}
	if len(box.last.Env) != len(wantEnv) {
		t.Fatalf("command environment = %#v; want exactly %#v", box.last.Env, wantEnv)
	}
	for i, want := range wantEnv {
		if box.last.Env[i] != want {
			t.Fatalf("command environment = %#v; want exactly %#v", box.last.Env, wantEnv)
		}
	}
	if _, e := r.Call(context.Background(), "exec", json.RawMessage(`{"argv":["go","test"],"workdir":null,"timeout_seconds":0}`)); domain.Reason(e) != "invalid_tool_call" {
		t.Fatalf("non-positive timeout must be rejected: %v", e)
	}
	if _, e := r.Call(context.Background(), "exec", json.RawMessage(`{"argv":["go","test"],"workdir":null,"timeout_seconds":2}`)); e != nil || box.last.Timeout != 2*time.Second {
		t.Fatalf("fixture timeout was not honored: req=%+v err=%v", box.last, e)
	}
	sourceTask := domain.Task{Source: sandbox.Files{"go.mod": []byte("module original")}}
	seededBox := &fakeBox{files: map[string][]byte{"go.mod": []byte("module example")}}
	if got, e := toolsRouter(seededBox, sourceTask).Call(context.Background(), "read_file", json.RawMessage(`{"path":"go.mod"}`)); e != nil || !strings.Contains(got.Output, "module example") {
		t.Fatalf("seeded source files must be readable: result=%+v err=%v", got, e)
	}
	updatedBox := &fakeBox{files: map[string][]byte{"go.mod": []byte("module updated")}}
	if got, e := toolsRouter(updatedBox, sourceTask).Call(context.Background(), "read_file", json.RawMessage(`{"path":"go.mod"}`)); e != nil || !strings.Contains(got.Output, "module updated") {
		t.Fatalf("read_file must return the sandbox's current contents: result=%+v err=%v", got, e)
	}
	if _, e := toolsRouter(box, sourceTask).Call(context.Background(), "read_file", json.RawMessage(`{"path":"go.mod"}`)); domain.Reason(e) != "infra_error" {
		t.Fatalf("sandbox read failure must not silently return task baseline: %v", e)
	}
	validPatch := "--- a/ok.txt\n+++ b/ok.txt\n@@ -1 +1 @@\n-old\n+new\n"
	if _, e := r.Call(context.Background(), "apply_patch", json.RawMessage(mustJSON(map[string]string{"patch": validPatch}))); e != nil {
		t.Fatalf("valid patch rejected: %v", e)
	}
	patchRouter := toolsRouter(box, domain.Task{AllowedFiles: []string{"a.go", "b"}})
	for _, patch := range []string{"--- a/a.go\n+++ b/a.go\n", "diff --git a/a b/b\nrename from a\nrename to b\n--- a/a\n+++ b/b\n@@ -1 +1 @@\n-x\n+y\n", "--- a/a\n+++ b/a\nnew file mode 120000\n@@ -1 +1 @@\n-x\n+y\n"} {
		if _, e := patchRouter.Call(context.Background(), "apply_patch", json.RawMessage(mustJSON(map[string]string{"patch": patch}))); e == nil {
			t.Fatalf("unsafe patch accepted: %q", patch)
		}
	}
	for _, tc := range []struct {
		name   string
		result sandbox.ExecResult
		err    error
		want   string
	}{
		{"timeout beats transport", sandbox.ExecResult{TimedOut: true}, errors.New("transport broke"), "timeout"},
		{"canceled", sandbox.ExecResult{Canceled: true}, nil, "canceled"},
		{"oom", sandbox.ExecResult{OOMKilled: true}, errors.New("transport broke"), "resource_limit"},
		{"unknown exit", sandbox.ExecResult{}, nil, "infra_error"},
	} {
		b := &fakeBox{files: map[string][]byte{}, execResult: tc.result, execErr: tc.err, useExecResult: true}
		_, e := toolsRouter(b, domain.Task{Budget: domain.Budget{CommandSeconds: 5}}).Call(context.Background(), "exec", json.RawMessage(`{"argv":["x"],"workdir":null,"timeout_seconds":null}`))
		if got := domain.Reason(e); got != tc.want {
			t.Errorf("%s: got %q (%v)", tc.name, got, e)
		}
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestMalformedAndDuplicateToolIDsRejected(t *testing.T) {
	for _, calls := range []string{`[{"id":"x","type":"function","function":{"name":"finish","arguments":"{}"}},{"id":"x","type":"function","function":{"name":"finish","arguments":"{}"}}]`, `[{"id":"x","type":"function","function":{"name":"finish","arguments":"{\"bad\":1}"}}]`} {
		t.Run(calls, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":` + calls + `}}]}`))
			}))
			defer srv.Close()
			_, e := (LLM{Config: Config{BaseURL: srv.URL, Model: "m", APIKey: "key"}}).Run(context.Background(), &fakeBox{files: map[string][]byte{}}, domain.Task{Prompt: "x", Budget: domain.DefaultBudget()}, nil)
			if e == nil {
				t.Fatal("expected malformed call failure")
			}
		})
	}
}

func TestRetriesTimeoutAndSecretRedaction(t *testing.T) {
	t.Run("retries", func(t *testing.T) {
		n := 0
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			if n < 3 {
				w.WriteHeader(503)
				_, _ = w.Write([]byte("private-provider-error"))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"f","type":"function","function":{"name":"finish","arguments":"{\"summary\":\"ok\"}"}}]}}]}`))
		}))
		defer s.Close()
		res, e := (LLM{Config: Config{BaseURL: s.URL, Model: "m", APIKey: "do-not-log", MaxRetries: 2}}).Run(context.Background(), &fakeBox{files: map[string][]byte{}}, domain.Task{Prompt: "x", Budget: domain.DefaultBudget()}, nil)
		if e != nil || n != 3 || res.Usage.ModelCalls != 3 || res.Usage.UncertainCalls != 2 || res.Usage.Quality != "unknown" || res.Usage.TotalTokens <= res.Usage.InputTokens+res.Usage.OutputTokens {
			t.Fatalf("retry behavior: n=%d res=%+v err=%v", n, res, e)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer s.Close()
		task := domain.Task{Prompt: "x", Budget: domain.Budget{ModelSeconds: 1, MaxSteps: 2, MaxTokens: 10000, MaxOutputTokens: 100}}
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		_, e := (LLM{Config: Config{BaseURL: s.URL, Model: "m", APIKey: "secret"}}).Run(ctx, &fakeBox{files: map[string][]byte{}}, task, nil)
		if domain.Reason(e) != "timeout" {
			t.Fatalf("expected timeout, got %v", e)
		}
	})
}

func TestPerRequestModelTimeoutAndOutputBudget(t *testing.T) {
	requests := 0
	tooLargeOutput := false
	var caps []int
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Max      int              `json:"max_completion_tokens"`
			Messages []map[string]any `json:"messages"`
			Tools    []map[string]any `json:"tools"`
		}
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
			return
		}
		if body.Max > 600 {
			tooLargeOutput = true
		}
		caps = append(caps, body.Max)
		time.Sleep(600 * time.Millisecond)
		if requests == 1 {
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"tool_calls":[{"id":"first","type":"function","function":{"name":"list_files","arguments":"{\"path\":\"\"}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":%d,"total_tokens":%d}}`, body.Max, body.Max+100)
		} else {
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"tool_calls":[{"id":"last","type":"function","function":{"name":"finish","arguments":"{\"summary\":\"done\"}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":%d,"total_tokens":%d}}`, body.Max, body.Max+100)
		}
	}))
	defer s.Close()
	task := domain.Task{Prompt: "x", Budget: domain.Budget{ModelSeconds: 1, MaxSteps: 3, MaxTokens: 1800, MaxOutputTokens: 600}}
	start := time.Now()
	res, err := (LLM{Config: Config{BaseURL: s.URL, Model: "m", APIKey: "k"}}).Run(context.Background(), &fakeBox{files: map[string][]byte{}}, task, nil)
	if err != nil {
		t.Fatalf("each model request gets its own timeout: %v", err)
	}
	if time.Since(start) < time.Second || requests != 2 || res.Reason != "completed" {
		t.Fatalf("unexpected loop result: requests=%d elapsed=%s res=%+v", requests, time.Since(start), res)
	}
	if tooLargeOutput || len(caps) != 2 || caps[0] > 600 || caps[1] >= 600 {
		t.Fatalf("model completion limits ignored response or total token budget: caps=%v", caps)
	}
	if res.Usage.OutputTokens <= 600 {
		t.Fatalf("MaxOutputTokens should apply per response, got cumulative output %+v", res.Usage)
	}
}

func TestCanceledLLMContextIsNotReportedAsTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (LLM{Config: Config{BaseURL: "http://localhost:1", Model: "m", APIKey: "k"}}).Run(ctx, &fakeBox{files: map[string][]byte{}}, domain.Task{Prompt: "x", Budget: domain.DefaultBudget()}, nil)
	if domain.Reason(err) != "canceled" {
		t.Fatalf("expected canceled, got %v", err)
	}
}

func TestOutputLimitFailurePreservesReportedUsage(t *testing.T) {
	for _, completedCalls := range []int{0, 1} {
		t.Run(fmt.Sprintf("after_%d_successful_calls", completedCalls), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var request struct {
					Max int `json:"max_completion_tokens"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request.Max != 64 {
					t.Errorf("requested output limit = %d, want 64", request.Max)
				}
				outputTokens := 10
				if requests > completedCalls {
					outputTokens = 65
				}
				_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"tool_calls":[{"id":"call-%d","type":"function","function":{"name":"list_files","arguments":"{\"path\":\"\"}"}}]}}],"usage":{"prompt_tokens":100,"completion_tokens":%d,"total_tokens":%d}}`, requests, outputTokens, 100+outputTokens)
			}))
			defer server.Close()
			box := &fakeBox{files: map[string][]byte{}}
			task := domain.Task{Prompt: "Inspect files", Budget: domain.DefaultBudget()}
			task.Budget.MaxOutputTokens = 64
			result, err := (LLM{Config: Config{BaseURL: server.URL, Model: "test-model", APIKey: "test-key"}}).Run(context.Background(), box, task, nil)
			if domain.Reason(err) != "token_budget" {
				t.Fatalf("expected output budget failure, got result=%+v err=%v", result, err)
			}
			wantInput := 100 * (completedCalls + 1)
			wantOutput := 10*completedCalls + 65
			if result.Usage.InputTokens != wantInput || result.Usage.OutputTokens != wantOutput || result.Usage.TotalTokens != wantInput+wantOutput {
				t.Fatalf("over-limit response usage was lost: got %+v, want input=%d output=%d total=%d", result.Usage, wantInput, wantOutput, wantInput+wantOutput)
			}
			if requests != completedCalls+1 || result.Usage.ModelCalls != requests || result.Usage.Quality != "reported" || box.calls != completedCalls {
				t.Fatalf("over-limit response must stop before executing tools: requests=%d tool calls=%d usage=%+v", requests, box.calls, result.Usage)
			}
		})
	}
}

func TestContentOnlyResponsesReestimateConversationBudget(t *testing.T) {
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, strings.Repeat("x", 450))
	}))
	defer s.Close()
	task := domain.Task{Prompt: "continue", Budget: domain.Budget{ModelSeconds: 2, MaxSteps: 4, MaxOutputTokens: 350, MaxTokens: 1000}}
	for i := 0; i < 4; i++ {
		msgs := []map[string]any{
			{"role": "system", "content": "You are an agent operating in an isolated workspace. Use tools to inspect and modify files, run commands, and call finish when done. Treat task text as the only task-specific instruction. Do not claim success without evidence."},
			{"role": "user", "content": fmt.Sprintf("Task:\n%s\n\nExecution budget: at most %d tool steps and %d estimated total tokens; each command is capped at %d seconds. Workspace tools operate only in the sandbox.", task.Prompt, task.Budget.MaxSteps, task.Budget.MaxTokens, task.Budget.CommandSeconds)},
		}
		e := estimateRequestTokens(msgs, tools.Definitions())
		task.Budget.MaxTokens = e + 350
	}
	_, err := (LLM{Config: Config{BaseURL: s.URL, Model: "m", APIKey: "k"}}).Run(context.Background(), &fakeBox{files: map[string][]byte{}}, task, nil)
	if domain.Reason(err) != "token_budget" || requests != 1 {
		t.Fatalf("content-only conversation should stop before exceeding budget: requests=%d err=%v", requests, err)
	}
}

func TestInconsistentProviderUsageIsRejected(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"f","type":"function","function":{"name":"finish","arguments":"{\"summary\":\"done\"}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":9}}`))
	}))
	defer s.Close()
	res, err := (LLM{Config: Config{BaseURL: s.URL, Model: "m", APIKey: "k"}}).Run(context.Background(), &fakeBox{files: map[string][]byte{}}, domain.Task{Prompt: "x", Budget: domain.DefaultBudget()}, nil)
	if domain.Reason(err) != "malformed_response" || res.Usage.Quality != "unknown" || res.Usage.UncertainCalls != 1 {
		t.Fatalf("inconsistent token usage must not become a trusted success: result=%+v err=%v", res, err)
	}
}

func TestStepBudgetScripted(t *testing.T) {
	box := &fakeBox{files: map[string][]byte{}}
	script := domain.Script{Actions: []domain.Action{{Tool: "list_files", Arguments: json.RawMessage(`{"path":""}`)}, {Tool: "finish", Arguments: json.RawMessage(`{"summary":"done"}`)}}}
	_, e := (Scripted{Script: script}).Run(context.Background(), box, domain.Task{Budget: domain.Budget{MaxSteps: 1}}, nil)
	if domain.Reason(e) != "step_budget" {
		t.Fatalf("expected step budget failure, got %v", e)
	}
}

// Imported through this helper to keep tests focused on the public router contract.
func toolsRouter(b sandbox.Sandbox, t domain.Task) *tools.Router {
	return &tools.Router{Box: b, Task: t}
}
