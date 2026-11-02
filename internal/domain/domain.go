package domain

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"strings"
	"time"
)

type Budget struct {
	RunSeconds      int `json:"run_seconds"`
	PrepareSeconds  int `json:"prepare_seconds"`
	CommandSeconds  int `json:"command_seconds"`
	ModelSeconds    int `json:"model_seconds"`
	CollectSeconds  int `json:"collect_seconds"`
	EvalSeconds     int `json:"eval_seconds"`
	CleanupSeconds  int `json:"cleanup_seconds"`
	MaxSteps        int `json:"max_steps"`
	MaxTokens       int `json:"max_tokens"`
	MaxOutputTokens int `json:"max_output_tokens"`
}

func DefaultBudget() Budget { return Budget{600, 120, 30, 60, 30, 120, 30, 20, 30000, 4096} }

type Action struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type Script struct {
	Actions []Action `json:"actions"`
}

type Grader struct {
	Kind       string `json:"kind"`
	EntryPoint string `json:"entrypoint"`
}

// ExecutionEnvironment selects trusted, image-owned offline dependencies.
// Host paths and arbitrary environment variables are deliberately not accepted.
type ExecutionEnvironment struct {
	GoModuleCache string `json:"go_module_cache,omitempty"`
}

type HarborSpec struct {
	VerifierTmpfs   []string `json:"verifier_tmpfs,omitempty"`
	PackageDir      string   `json:"package_dir"`
	PackageSHA256   string   `json:"package_sha256"`
	AgentImage      string   `json:"agent_image"`
	VerifierImage   string   `json:"verifier_image"`
	Workdir         string   `json:"workdir"`
	AgentEnv        []string `json:"agent_env,omitempty"`
	VerifierEnv     []string `json:"verifier_env,omitempty"`
	RequiredOutputs []string `json:"required_outputs"`
	PassThreshold   float64  `json:"pass_threshold"`
}

type Task struct {
	Backend         string               `json:"backend,omitempty"`
	Harbor          *HarborSpec          `json:"harbor,omitempty"`
	Metadata        map[string]string    `json:"metadata,omitempty"`
	Environment     ExecutionEnvironment `json:"environment,omitempty"`
	Grader          *Grader              `json:"grader,omitempty"`
	SchemaVersion   int                  `json:"schema_version"`
	ID              string               `json:"task_id"`
	Revision        int                  `json:"task_revision"`
	Prompt          string               `json:"prompt"`
	SourceDir       string               `json:"source_dir"`
	SourceSHA256    string               `json:"source_sha256"`
	EvaluatorDir    string               `json:"evaluator_dir"`
	EvaluatorSHA256 string               `json:"evaluator_sha256"`
	RequiredTests   []string             `json:"required_tests"`
	AllowedFiles    []string             `json:"allowed_files"`
	Budget          Budget               `json:"budget"`
	Limits          sandbox.Limits       `json:"limits"`
	Scripts         map[string]Script    `json:"scripts"`
	Source          sandbox.Files        `json:"-"`
	Evaluator       sandbox.Files        `json:"-"`
}

func (t Task) CommandEnvironment() []string {
	if t.IsHarbor() {
		env := append([]string(nil), t.Harbor.AgentEnv...)
		for _, fallback := range []string{"PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1"} {
			key, _, _ := strings.Cut(fallback, "=")
			found := false
			for _, item := range env {
				if strings.HasPrefix(item, key+"=") {
					found = true
					break
				}
			}
			if !found {
				env = append(env, fallback)
			}
		}
		return env
	}
	env := []string{"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off"}
	if t.Environment.GoModuleCache != "" {
		env = append(env, "GOMODCACHE="+t.Environment.GoModuleCache)
	}
	return env
}

type Event struct {
	Sequence int       `json:"sequence"`
	Time     time.Time `json:"time"`
	Phase    string    `json:"phase"`
	Type     string    `json:"type"`
	Data     any       `json:"data,omitempty"`
}

type Usage struct {
	ModelCalls     int    `json:"model_calls"`
	ToolCalls      int    `json:"tool_calls"`
	Steps          int    `json:"steps"`
	InputTokens    int    `json:"input_tokens"`
	OutputTokens   int    `json:"output_tokens"`
	TotalTokens    int    `json:"total_tokens"`
	Quality        string `json:"quality"`
	UncertainCalls int    `json:"uncertain_calls"`
}

type AgentResult struct {
	Reason  string `json:"reason"`
	Summary string `json:"summary,omitempty"`
	Usage   Usage  `json:"usage"`
}

type Emit func(phase, event string, data any)

type Agent interface {
	Run(context.Context, sandbox.Sandbox, Task, Emit) (AgentResult, error)
}

type Failure struct {
	Reason string
	Err    error
}

func (f *Failure) Error() string {
	if f.Err == nil {
		return f.Reason
	}
	return f.Reason + ": " + f.Err.Error()
}
func (f *Failure) Unwrap() error          { return f.Err }
func Fail(reason string, err error) error { return &Failure{Reason: reason, Err: err} }
func Reason(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Reason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "infra_error"
}

func (t Task) IsHarbor() bool { return t.Backend == "harbor" && t.Harbor != nil }
func (t Task) Workdir() string {
	if t.IsHarbor() {
		return t.Harbor.Workdir
	}
	return "/workspace"
}
