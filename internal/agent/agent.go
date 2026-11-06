package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/tools"
)

type Scripted struct{ Script domain.Script }

func (s Scripted) Run(ctx context.Context, box sandbox.Sandbox, task domain.Task, emit domain.Emit) (domain.AgentResult, error) {
	return runActions(ctx, box, task, emit, s.Script.Actions, domain.Usage{Quality: "known"})
}

type Config struct {
	BaseURL             string
	Model               string
	APIKey              string
	HTTPClient          *http.Client
	MaxCompletionTokens int
	MaxRetries          int
}
type LLM struct{ Config Config }

func (a LLM) Run(ctx context.Context, box sandbox.Sandbox, task domain.Task, emit domain.Emit) (domain.AgentResult, error) {
	c := a.Config
	if strings.TrimSpace(c.Model) == "" {
		return domain.AgentResult{}, domain.Fail("configuration_error", errors.New("model is required"))
	}
	if c.APIKey == "" {
		return domain.AgentResult{}, domain.Fail("configuration_error", errors.New("API key is required"))
	}
	endpoint, e := endpoint(c.BaseURL)
	if e != nil {
		return domain.AgentResult{}, domain.Fail("configuration_error", e)
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: time.Duration(max(1, task.Budget.ModelSeconds)) * time.Second}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	limit := task.Budget.ModelSeconds
	if limit <= 0 {
		limit = 60
	}
	maxSteps := task.Budget.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 20
	}
	maxTokens := task.Budget.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 30000
	}
	outBudget := task.Budget.MaxOutputTokens
	if outBudget <= 0 {
		outBudget = 4096
	}
	completion := c.MaxCompletionTokens
	if completion <= 0 || completion > outBudget {
		completion = outBudget
	}
	usage := domain.Usage{Quality: "reported"}
	uncertainTokenReserve := 0
	router := tools.Router{Box: box, Task: task}
	toolDefinitions := tools.DefinitionsForTask(task)
	seenToolIDs := map[string]bool{}
	msgs := []map[string]any{{"role": "system", "content": "You are an agent operating in an isolated workspace. Use tools to inspect and modify files, run commands, and call finish when done. Treat task text as the only task-specific instruction. Do not claim success without evidence."}, {"role": "user", "content": fmt.Sprintf("Task:\n%s\n\nExecution budget: at most %d tool steps and %d estimated total tokens; each command is capped at %d seconds. Workspace tools operate only in the sandbox.", task.Prompt, maxSteps, maxTokens, task.Budget.CommandSeconds)}}
	inputEstimate := estimateRequestTokens(msgs, toolDefinitions)
	if inputEstimate > maxTokens {
		return domain.AgentResult{}, domain.Fail("token_budget", errors.New("initial prompt exceeds token budget"))
	}
	for callNo := 1; callNo <= maxSteps; callNo++ {
		inputEstimate = estimateRequestTokens(msgs, toolDefinitions)
		if err := ctx.Err(); err != nil {
			return domain.AgentResult{Usage: usage}, domain.Fail(domain.Reason(err), err)
		}
		if usage.TotalTokens+inputEstimate >= maxTokens {
			return domain.AgentResult{Usage: usage}, domain.Fail("token_budget", errors.New("token budget exhausted"))
		}
		remainingOutput := outBudget
		if remainingOutput <= 0 {
			return domain.AgentResult{Usage: usage}, domain.Fail("token_budget", errors.New("output token budget exhausted"))
		}
		retries := c.MaxRetries
		if retries < 0 {
			retries = 0
		}
		if retries > 2 {
			retries = 2
		}
		remainingTotal := maxTokens - usage.TotalTokens
		perAttemptAllowance := inputEstimate + completion
		if perAttemptAllowance > 0 && retries > 0 {
			maxByBudget := remainingTotal/perAttemptAllowance - 1
			if maxByBudget < retries {
				retries = maxByBudget
			}
		}
		if retries < 0 {
			retries = 0
		}
		callCompletion := completion
		if callCompletion > remainingOutput {
			callCompletion = remainingOutput
		}
		if room := remainingTotal/(retries+1) - inputEstimate; callCompletion > room {
			callCompletion = room
		}
		if callCompletion <= 0 {
			return domain.AgentResult{Usage: usage}, domain.Fail("token_budget", errors.New("insufficient token budget for model request"))
		}
		onAttempt := func(attempt int) {
			usage.ModelCalls++
			emitTrace(emit, "model_call", map[string]any{"call": usage.ModelCalls, "attempt": attempt + 1, "model": c.Model}, c.APIKey)
		}
		onUncertain := func() {
			usage.UncertainCalls++
			usage.Quality = "unknown"
			uncertainTokenReserve += inputEstimate + callCompletion
			usage.TotalTokens += inputEstimate + callCompletion
		}
		reqCtx, reqCancel := context.WithTimeout(ctx, time.Duration(limit)*time.Second)
		resp, known, err := request(reqCtx, &clientCopy, endpoint, c.APIKey, c.Model, msgs, toolDefinitions, callCompletion, retries, onAttempt, onUncertain)
		reqCancel()
		uncertainCall := !known
		if !known && usage.Quality != "unknown" {
			usage.Quality = "unknown"
		}
		if err != nil {
			return domain.AgentResult{Usage: usage}, err
		}
		if resp.Usage != nil {
			if resp.Usage.PromptTokens < 0 || resp.Usage.CompletionTokens < 0 || resp.Usage.TotalTokens < 0 || resp.Usage.TotalTokens != resp.Usage.PromptTokens+resp.Usage.CompletionTokens {
				usage.UncertainCalls++
				usage.Quality = "unknown"
				usage.TotalTokens += inputEstimate + callCompletion
				return domain.AgentResult{Usage: usage}, domain.Fail("malformed_response", errors.New("provider token usage is inconsistent"))
			}
			usage.InputTokens += resp.Usage.PromptTokens
			usage.OutputTokens += resp.Usage.CompletionTokens
			usage.TotalTokens += resp.Usage.TotalTokens
			if resp.Usage.CompletionTokens > callCompletion {
				return domain.AgentResult{Usage: usage}, domain.Fail("token_budget", errors.New("completion exceeded requested output limit"))
			}
		} else {
			usage.InputTokens += inputEstimate
			outBytes, _ := json.Marshal(resp.Choices)
			usage.OutputTokens += (len(outBytes) + 2) / 3
			usage.TotalTokens = usage.InputTokens + usage.OutputTokens + uncertainTokenReserve
			if !uncertainCall && usage.Quality == "reported" {
				usage.Quality = "estimated"
			}
		}
		if usage.TotalTokens > maxTokens {
			return domain.AgentResult{Usage: usage}, domain.Fail("token_budget", errors.New("reported model usage exceeded token budget"))
		}
		if len(resp.Choices) != 1 {
			return domain.AgentResult{Usage: usage}, domain.Fail("provider_error", errors.New("expected one completion choice"))
		}
		m := resp.Choices[0].Message
		if m.Content != nil {
			emitTrace(emit, "model_response", map[string]any{"call": usage.ModelCalls, "summary": bounded([]byte(*m.Content), 1024), "usage": usage}, c.APIKey)
		}
		assistant := map[string]any{"role": "assistant"}
		if m.Content != nil {
			assistant["content"] = *m.Content
		}
		calls := m.ToolCalls
		if len(calls) == 0 {
			if strings.TrimSpace(content(m.Content)) != "" {
				msgs = append(msgs, assistant)
				continue
			}
			return domain.AgentResult{Usage: usage}, domain.Fail("malformed_response", errors.New("assistant returned neither content nor tool calls"))
		}
		wireCalls := make([]map[string]any, 0, len(calls))
		for _, tc := range calls {
			if tc.ID == "" || seenToolIDs[tc.ID] {
				return domain.AgentResult{Usage: usage}, domain.Fail("malformed_response", errors.New("empty or duplicate tool_call_id"))
			}
			seenToolIDs[tc.ID] = true
			if tc.Type != "function" || tc.Function.Name == "" {
				return domain.AgentResult{Usage: usage}, domain.Fail("malformed_response", errors.New("invalid function tool call"))
			}
			wireCalls = append(wireCalls, map[string]any{"id": tc.ID, "type": "function", "function": map[string]string{"name": tc.Function.Name, "arguments": tc.Function.Arguments}})
		}
		assistant["tool_calls"] = wireCalls
		msgs = append(msgs, assistant)
		pendingImages := make([]any, 0)
		for _, tc := range calls {
			if usage.Steps >= maxSteps {
				return domain.AgentResult{Usage: usage}, domain.Fail("step_budget", errors.New("tool step budget exhausted"))
			}
			if err := ctx.Err(); err != nil {
				return domain.AgentResult{Usage: usage}, domain.Fail(domain.Reason(err), err)
			}
			usage.Steps++
			usage.ToolCalls++
			emitTrace(emit, "tool_call", map[string]any{"step": usage.Steps, "tool": tc.Function.Name, "arguments": safeArguments(json.RawMessage(tc.Function.Arguments), c.APIKey)}, c.APIKey)
			tr, e := router.Call(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			if e != nil {
				emitTrace(emit, "tool_error", map[string]any{"step": usage.Steps, "tool": tc.Function.Name, "reason": domain.Reason(e)}, c.APIKey)
				return domain.AgentResult{Usage: usage}, e
			}
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": tc.ID, "content": tr.Output})
			if tr.ImageDataURL != "" {
				pendingImages = append(pendingImages, map[string]any{"type": "image_url", "image_url": map[string]any{"url": tr.ImageDataURL}})
			}
			emitTrace(emit, "tool_result", map[string]any{"step": usage.Steps, "tool": tr.Name, "result": tr.Output, "usage": usage}, c.APIKey)
			if tr.Finished {
				if emit != nil {
					emit("agent", "finished", map[string]any{"steps": usage.Steps})
				}
				return domain.AgentResult{Reason: "completed", Summary: tr.Output, Usage: usage}, nil
			}
		}
		if len(pendingImages) > 0 {
			msgs = append(msgs, map[string]any{"role": "user", "content": pendingImages})
		}
		inputEstimate = estimateRequestTokens(msgs, toolDefinitions)
		if usage.TotalTokens+inputEstimate > maxTokens {
			return domain.AgentResult{Usage: usage}, domain.Fail("token_budget", errors.New("conversation exceeds token budget"))
		}
	}
	return domain.AgentResult{Usage: usage}, domain.Fail("step_budget", errors.New("maximum model/tool steps exhausted"))
}

func runActions(ctx context.Context, box sandbox.Sandbox, task domain.Task, emit domain.Emit, actions []domain.Action, u domain.Usage) (domain.AgentResult, error) {
	maxSteps := task.Budget.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 20
	}
	router := tools.Router{Box: box, Task: task}
	for _, act := range actions {
		if err := ctx.Err(); err != nil {
			return domain.AgentResult{Usage: u}, domain.Fail(domain.Reason(err), err)
		}
		if u.Steps >= maxSteps {
			return domain.AgentResult{Usage: u}, domain.Fail("step_budget", errors.New("tool step budget exhausted"))
		}
		u.Steps++
		u.ToolCalls++
		emitTrace(emit, "tool_call", map[string]any{"step": u.Steps, "tool": act.Tool, "arguments": safeArguments(act.Arguments, "")}, "")
		r, e := router.Call(ctx, act.Tool, act.Arguments)
		if e != nil {
			emitTrace(emit, "tool_error", map[string]any{"step": u.Steps, "tool": act.Tool, "reason": domain.Reason(e)}, "")
			return domain.AgentResult{Usage: u}, e
		}
		emitTrace(emit, "tool_result", map[string]any{"step": u.Steps, "tool": r.Name, "result": r.Output, "usage": u}, "")
		if r.Finished {
			if emit != nil {
				emit("agent", "finished", map[string]any{"steps": u.Steps})
			}
			return domain.AgentResult{Reason: "completed", Summary: r.Output, Usage: u}, nil
		}
	}
	return domain.AgentResult{Reason: "script_exhausted", Usage: u}, nil
}

func endpoint(raw string) (string, error) {
	if raw == "" {
		raw = "https://api.openai.com/v1"
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil {
		return "", errors.New("base URL must be an absolute URL without userinfo")
	}
	if u.Scheme != "https" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if u.Scheme != "http" || !(host == "localhost" || ip != nil && ip.IsLoopback()) {
			return "", errors.New("base URL must use HTTPS (HTTP is allowed only for loopback mocks)")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}
func estimateRequestTokens(ms []map[string]any, defs []map[string]any) int {
	messages := make([]any, len(ms))
	imageCount := 0
	for i, message := range ms {
		messages[i], imageCount = replaceImagePayloads(message, imageCount)
	}
	b, _ := json.Marshal(struct {
		Messages []any            `json:"messages"`
		Tools    []map[string]any `json:"tools"`
	}{messages, defs})
	return (len(b)+2)/3 + 128 + imageCount*tools.ImageTokenBudget
}

func replaceImagePayloads(value any, count int) (any, int) {
	switch current := value.(type) {
	case map[string]any:
		copy := make(map[string]any, len(current))
		for key, item := range current {
			if key == "image_url" {
				if image, ok := item.(map[string]any); ok {
					if url, ok := image["url"].(string); ok && strings.HasPrefix(url, "data:image/") {
						copy[key] = map[string]any{"url": "[image payload omitted]"}
						count++
						continue
					}
				}
			}
			copy[key], count = replaceImagePayloads(item, count)
		}
		return copy, count
	case []any:
		copy := make([]any, len(current))
		for i := range current {
			copy[i], count = replaceImagePayloads(current[i], count)
		}
		return copy, count
	default:
		return value, count
	}
}
func bounded(b []byte, limit int) string {
	s := strings.ToValidUTF8(string(b), "�")
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit]) + "…[truncated]"
	}
	return s
}
func safeArguments(raw json.RawMessage, apiKey string) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return bounded(raw, 2048)
	}
	return sanitizeTraceValue(v, apiKey)
}
func sanitizeTraceValue(v any, apiKey string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			if sensitiveTraceKey(k) {
				out[k] = "[redacted]"
			} else {
				out[k] = sanitizeTraceValue(value, apiKey)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = sanitizeTraceValue(x[i], apiKey)
		}
		return out
	case string:
		return redactText(x, apiKey)
	default:
		return v
	}
}

func sensitiveTraceKey(k string) bool {
	lk := strings.ToLower(strings.ReplaceAll(k, "-", "_"))
	return strings.Contains(lk, "secret") || strings.Contains(lk, "password") || strings.Contains(lk, "credential") || strings.Contains(lk, "private_key") || lk == "token" || (strings.HasSuffix(lk, "_token") && lk != "total_tokens") || strings.Contains(lk, "api_key") || strings.Contains(lk, "apikey") || lk == "authorization" || lk == "content" || lk == "patch"
}

var credentialPattern = regexp.MustCompile(`(?i)(api[_-]?key|password|secret|credential|private[_-]?key|access[_-]?token|authorization|\btoken\b)(["'=:\s]+)([^\s,;"}]+)`)
var rawCredentialPattern = regexp.MustCompile(`(?i)\b(sk-[A-Za-z0-9_-]{10,}|AKIA[0-9A-Z]{16}|Bearer\s+[A-Za-z0-9._~+/-]{8,})\b`)

func redactText(s, apiKey string) string {
	if apiKey != "" {
		s = strings.ReplaceAll(s, apiKey, "[redacted]")
	}
	s = credentialPattern.ReplaceAllString(s, "$1$2[redacted]")
	return rawCredentialPattern.ReplaceAllString(s, "[redacted]")
}
func emitTrace(emit domain.Emit, event string, data any, apiKey string) {
	if emit == nil {
		return
	}
	b, _ := json.Marshal(sanitizeTraceValue(data, apiKey))
	var safe any
	if json.Unmarshal(b, &safe) != nil {
		safe = "[unavailable]"
	}
	// A second text pass catches credentials embedded in command output or model text.
	b, _ = json.Marshal(safe)
	if len(b) > 192<<10 {
		safe = map[string]any{"truncated": true, "preview": bounded(b, 4096)}
	}
	emit("agent", event, safe)
}
func content(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type completionResponse struct {
	Choices []struct {
		Message struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func request(ctx context.Context, client *http.Client, endpoint, key, model string, msgs []map[string]any, defs []map[string]any, maxTokens, maxRetries int, onAttempt func(int), onUncertain func()) (completionResponse, bool, error) {
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries > 2 {
		maxRetries = 2
	}
	body := map[string]any{"model": model, "messages": msgs, "tools": defs, "tool_choice": "auto", "parallel_tool_calls": false, "max_completion_tokens": maxTokens}
	payload, e := json.Marshal(body)
	if e != nil {
		return completionResponse{}, true, domain.Fail("provider_error", e)
	}
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if onAttempt != nil {
			onAttempt(attempt)
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if e != nil {
			return completionResponse{}, true, domain.Fail("provider_error", e)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		res, e := client.Do(req)
		if e != nil {
			if onUncertain != nil {
				onUncertain()
			}
			if ctx.Err() != nil {
				return completionResponse{}, false, domain.Fail(domain.Reason(ctx.Err()), ctx.Err())
			}
			var netErr net.Error
			if errors.As(e, &netErr) && netErr.Timeout() {
				return completionResponse{}, false, domain.Fail("timeout", context.DeadlineExceeded)
			}
			if attempt < maxRetries {
				if !pause(ctx, time.Duration(attempt+1)*150*time.Millisecond) {
					return completionResponse{}, false, domain.Fail(domain.Reason(ctx.Err()), ctx.Err())
				}
				continue
			}
			return completionResponse{}, false, domain.Fail("provider_transport", errors.New("request failed after retries"))
		}
		data, readErr := io.ReadAll(io.LimitReader(res.Body, 2<<20+1))
		res.Body.Close()
		if len(data) > 2<<20 {
			if onUncertain != nil {
				onUncertain()
			}
			return completionResponse{}, true, domain.Fail("provider_error", errors.New("response exceeds 2 MiB"))
		}
		if readErr != nil {
			if onUncertain != nil {
				onUncertain()
			}
			if ctx.Err() != nil {
				return completionResponse{}, false, domain.Fail(domain.Reason(ctx.Err()), ctx.Err())
			}
			return completionResponse{}, false, domain.Fail("provider_transport", errors.New("failed to read provider response"))
		}
		if res.StatusCode == http.StatusUnauthorized {
			return completionResponse{}, true, domain.Fail("provider_auth", errors.New("provider rejected credentials"))
		}
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			if onUncertain != nil {
				onUncertain()
			}
			if attempt < maxRetries {
				if !pause(ctx, time.Duration(attempt+1)*200*time.Millisecond) {
					return completionResponse{}, false, domain.Fail(domain.Reason(ctx.Err()), ctx.Err())
				}
				continue
			}
			return completionResponse{}, false, domain.Fail("provider_transport", fmt.Errorf("provider returned retryable HTTP %d", res.StatusCode))
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return completionResponse{}, true, domain.Fail("provider_error", fmt.Errorf("provider returned HTTP %d", res.StatusCode))
		}
		var out completionResponse
		if e = json.Unmarshal(data, &out); e != nil {
			if onUncertain != nil {
				onUncertain()
			}
			return completionResponse{}, true, domain.Fail("malformed_response", errors.New("provider returned invalid JSON"))
		}
		return out, attempt == 0, nil
	}
	return completionResponse{}, false, domain.Fail("provider_transport", errors.New("request failed"))
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
