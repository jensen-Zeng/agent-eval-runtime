// Package sandbox defines the execution boundary independently of Docker and LLMs.
package sandbox

import (
	"context"
	"time"
)

type Files map[string][]byte

type Limits struct {
	CPUs         float64 `json:"cpus"`
	MemoryMiB    int64   `json:"memory_mib"`
	Pids         int64   `json:"pids"`
	TmpMiB       int64   `json:"tmp_mib,omitempty"`
	WorkspaceMiB int64   `json:"workspace_mib"`
}

type Spec struct {
	Image  string
	Labels map[string]string
	Limits Limits
}

type Info struct {
	ServerVersion string `json:"server_version"`
	OSType        string `json:"os_type"`
	Architecture  string `json:"architecture"`
	CPUs          int    `json:"cpus"`
	MemoryBytes   int64  `json:"memory_bytes"`
	ImageID       string `json:"image_id,omitempty"`
}

type ExecRequest struct {
	Argv        []string      `json:"argv"`
	Workdir     string        `json:"workdir,omitempty"`
	Env         []string      `json:"env,omitempty"`
	Timeout     time.Duration `json:"-"`
	OutputLimit int           `json:"-"`
}

type ExecResult struct {
	ExitCode   int    `json:"exit_code"`
	ExitKnown  bool   `json:"exit_known"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	TimedOut   bool   `json:"timed_out"`
	Canceled   bool   `json:"canceled"`
	OOMKilled  bool   `json:"oom_killed"`
	DurationMS int64  `json:"duration_ms"`
}

type Stats struct {
	CPUSeconds  float64 `json:"cpu_seconds"`
	MemoryBytes uint64  `json:"memory_bytes"`
	OOMKilled   bool    `json:"oom_killed"`
}

type Sandbox interface {
	ID() string
	Exec(context.Context, ExecRequest) (ExecResult, error)
	ListFiles(context.Context, string) ([]string, error)
	ReadFile(context.Context, string) ([]byte, error)
	WriteFile(context.Context, string, []byte) error
	ApplyPatch(context.Context, string) error
	Snapshot(context.Context) (Files, error)
	Stats(context.Context) (Stats, error)
	Destroy(context.Context) error
}

type Factory interface {
	Create(context.Context, Spec, Files) (Sandbox, error)
	Doctor(context.Context) (Info, error)
}
