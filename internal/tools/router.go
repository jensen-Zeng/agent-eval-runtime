package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
)

const MaxFileBytes = 1 << 20
const MaxOutputBytes = 64 << 10

type Result struct {
	Name     string `json:"name"`
	Output   string `json:"output"`
	Finished bool   `json:"finished,omitempty"`
}
type Router struct {
	Box  sandbox.Sandbox
	Task domain.Task
}

func Definitions() []map[string]any {
	return []map[string]any{
		fn("list_files", "List workspace files", obj(map[string]any{"path": strSchema("relative directory; empty means workspace root")}, "path")),
		fn("read_file", "Read a UTF-8 or binary workspace file", obj(map[string]any{"path": strSchema("relative file path")}, "path")),
		fn("write_file", "Create or replace a workspace file", obj(map[string]any{"path": strSchema("relative file path"), "content": strSchema("complete file contents")}, "path", "content")),
		fn("apply_patch", "Apply a unified diff to workspace files", obj(map[string]any{"patch": strSchema("unified diff")}, "patch")),
		fn("exec", "Run a command inside the isolated workspace", obj(map[string]any{"argv": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1}, "workdir": map[string]any{"anyOf": []map[string]any{{"type": "string", "description": "relative workspace directory"}, {"type": "null"}}}, "timeout_seconds": map[string]any{"anyOf": []map[string]any{{"type": "integer", "minimum": 1}, {"type": "null"}}}}, "argv", "workdir", "timeout_seconds")),
		fn("finish", "Finish the task with a short summary", obj(map[string]any{"summary": strSchema("completion summary")}, "summary")),
	}
}
func fn(name, desc string, params map[string]any) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": desc, "parameters": params, "strict": true}}
}
func obj(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}
func strSchema(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func (r *Router) Call(ctx context.Context, name string, raw json.RawMessage) (Result, error) {
	if r.Box == nil {
		return Result{}, domain.Fail("infra_error", errors.New("sandbox unavailable"))
	}
	decode := func(dst any) error {
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.DisallowUnknownFields()
		if e := d.Decode(dst); e != nil {
			return e
		}
		if e := d.Decode(new(any)); e != io.EOF {
			return errors.New("trailing JSON")
		}
		return nil
	}
	bad := func(e error) (Result, error) { return Result{}, domain.Fail("invalid_tool_call", e) }
	switch name {
	case "list_files":
		var a struct {
			Path string `json:"path"`
		}
		if e := decode(&a); e != nil {
			return bad(e)
		}
		p, e := clean(a.Path, true)
		if e != nil {
			return bad(e)
		}
		xs, e := r.Box.ListFiles(ctx, p)
		if e != nil {
			return Result{}, domain.Fail("tool_error", e)
		}
		return output(name, xs), nil
	case "read_file":
		var a struct {
			Path string `json:"path"`
		}
		if e := decode(&a); e != nil {
			return bad(e)
		}
		p, e := clean(a.Path, false)
		if e != nil {
			return bad(e)
		}
		if !r.allowed(p) {
			if _, exists := r.Task.Source[p]; !exists {
				return bad(errors.New("path is outside allowed files and source files"))
			}
		}
		b, e := r.Box.ReadFile(ctx, p)
		if e != nil {
			return Result{}, domain.Fail("infra_error", e)
		}
		if len(b) > MaxFileBytes {
			return bad(errors.New("file exceeds 1 MiB"))
		}
		return output(name, string(b)), nil
	case "write_file":
		var a struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if e := decode(&a); e != nil {
			return bad(e)
		}
		p, e := clean(a.Path, false)
		if e != nil {
			return bad(e)
		}
		if !r.allowed(p) {
			return bad(errors.New("path is outside allowed files"))
		}
		if len([]byte(a.Content)) > MaxFileBytes {
			return bad(errors.New("file exceeds 1 MiB"))
		}
		if e = r.Box.WriteFile(ctx, p, []byte(a.Content)); e != nil {
			return Result{}, domain.Fail("tool_error", e)
		}
		return output(name, map[string]any{"written": p, "bytes": len(a.Content)}), nil
	case "apply_patch":
		var a struct {
			Patch string `json:"patch"`
		}
		if e := decode(&a); e != nil {
			return bad(e)
		}
		if len(a.Patch) > MaxFileBytes {
			return bad(errors.New("patch exceeds 1 MiB"))
		}
		if strings.TrimSpace(a.Patch) == "" {
			return bad(errors.New("empty patch"))
		}
		if e := r.validatePatchPaths(a.Patch); e != nil {
			return bad(e)
		}
		if e := r.Box.ApplyPatch(ctx, a.Patch); e != nil {
			return Result{}, domain.Fail("tool_error", e)
		}
		return output(name, map[string]any{"applied": true}), nil
	case "exec":
		var a struct {
			Argv           []string `json:"argv"`
			Workdir        *string  `json:"workdir"`
			TimeoutSeconds *int     `json:"timeout_seconds"`
		}
		if e := decode(&a); e != nil {
			return bad(e)
		}
		if len(a.Argv) == 0 || len(a.Argv) > 128 {
			return bad(errors.New("argv must contain 1 to 128 items"))
		}
		for _, v := range a.Argv {
			if strings.IndexByte(v, 0) >= 0 {
				return bad(errors.New("NUL in argv"))
			}
		}
		wd := ""
		if a.Workdir != nil {
			wd = *a.Workdir
		}
		if wd != "" {
			var e error
			wd, e = clean(wd, true)
			if e != nil {
				return bad(e)
			}
		}
		budgetSec := r.Task.Budget.CommandSeconds
		if budgetSec <= 0 {
			budgetSec = 30
		}
		sec := budgetSec
		if a.TimeoutSeconds != nil {
			if *a.TimeoutSeconds <= 0 {
				return bad(errors.New("timeout_seconds must be positive"))
			}
			if *a.TimeoutSeconds < sec {
				sec = *a.TimeoutSeconds
			}
		}
		if r.Task.Budget.RunSeconds > 0 && sec > r.Task.Budget.RunSeconds {
			sec = r.Task.Budget.RunSeconds
		}
		x, e := r.Box.Exec(ctx, sandbox.ExecRequest{Argv: a.Argv, Workdir: wd, Timeout: time.Duration(sec) * time.Second, OutputLimit: MaxOutputBytes, Env: r.Task.CommandEnvironment()})
		if x.TimedOut || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{}, domain.Fail("timeout", context.DeadlineExceeded)
		}
		if x.Canceled || errors.Is(ctx.Err(), context.Canceled) {
			return Result{}, domain.Fail("canceled", context.Canceled)
		}
		if x.OOMKilled {
			return Result{}, domain.Fail("resource_limit", errors.New("command killed by OOM"))
		}
		if e != nil {
			return Result{}, domain.Fail("infra_error", e)
		}
		if !x.ExitKnown {
			return Result{}, domain.Fail("infra_error", errors.New("command exit status is unknown"))
		}
		return output(name, map[string]any{"exit_code": x.ExitCode, "exit_known": x.ExitKnown, "stdout": x.Stdout, "stderr": x.Stderr, "truncated": x.Truncated, "duration_ms": x.DurationMS}), nil
	case "finish":
		var a struct {
			Summary string `json:"summary"`
		}
		if e := decode(&a); e != nil {
			return bad(e)
		}
		a.Summary = strings.TrimSpace(a.Summary)
		if len(a.Summary) > 4096 {
			return bad(errors.New("summary exceeds 4096 bytes"))
		}
		return Result{Name: name, Output: a.Summary, Finished: true}, nil
	default:
		return bad(fmt.Errorf("unknown tool %q", name))
	}
}
func output(name string, v any) Result {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > MaxOutputBytes {
		preview := s
		if len(preview) > 8<<10 {
			preview = preview[:8<<10]
		}
		b, _ = json.Marshal(map[string]any{"truncated": true, "preview": preview})
		s = string(b)
	}
	return Result{Name: name, Output: s}
}
func (r *Router) allowed(p string) bool {
	if len(r.Task.AllowedFiles) == 0 {
		return true
	}
	for _, x := range r.Task.AllowedFiles {
		q, e := clean(x, false)
		if e == nil && q == p {
			return true
		}
	}
	return false
}

func (r *Router) validatePatchPaths(patch string) error {
	oldCount, newCount, hunkCount := 0, 0, 0
	lastHeader := ""
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "@@") {
			hunkCount++
		}
		for _, forbidden := range []string{"rename from ", "rename to ", "copy from ", "copy to ", "old mode ", "new mode ", "new file mode ", "deleted file mode ", "GIT binary patch", "Binary files "} {
			if strings.HasPrefix(line, forbidden) {
				return errors.New("rename, binary, mode, and symlink patches are forbidden")
			}
		}
		var raw string
		if strings.HasPrefix(line, "--- ") {
			oldCount++
			lastHeader = "old"
			raw = strings.TrimSpace(strings.TrimPrefix(line, "--- "))
		} else if strings.HasPrefix(line, "+++ ") {
			newCount++
			if lastHeader != "old" {
				return errors.New("patch file headers must appear as --- then +++ pairs")
			}
			lastHeader = "new"
			raw = strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
		} else {
			continue
		}
		if raw == "/dev/null" {
			continue
		}
		if i := strings.IndexByte(raw, '\t'); i >= 0 {
			raw = raw[:i]
		}
		raw = strings.TrimPrefix(raw, "a/")
		raw = strings.TrimPrefix(raw, "b/")
		name, err := clean(raw, false)
		if err != nil {
			return err
		}
		if !r.allowed(name) {
			return fmt.Errorf("patch path %q is outside allowed files", name)
		}
	}
	if oldCount == 0 || oldCount != newCount || lastHeader != "new" || hunkCount == 0 {
		return errors.New("patch must contain validated file headers and at least one hunk")
	}
	return nil
}
func clean(s string, emptyRoot bool) (string, error) {
	if s == "" && emptyRoot {
		return "", nil
	}
	if s == "." && emptyRoot {
		return "", nil
	}
	if s == "" || strings.ContainsRune(s, 0) || strings.Contains(s, "\\") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.Contains(s, ":") {
		return "", errors.New("path must be relative and cannot contain drive, UNC, or absolute prefixes")
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return "", errors.New("parent path traversal is forbidden")
		}
	}
	p := path.Clean(s)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", errors.New("invalid relative path")
	}
	return p, nil
}
