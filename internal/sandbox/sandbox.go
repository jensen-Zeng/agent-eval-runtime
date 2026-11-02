// Package sandbox defines the execution boundary independently of Docker and LLMs.
package sandbox

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
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
	Image          string
	Labels         map[string]string
	Limits         Limits
	Workdir        string
	ImageWorkspace bool
	Harbor         bool
	Env            []string
	FileModes      map[string]uint32
	WritableDirs   []string
}

var writableDirName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

var reservedWritableDirs = map[string]struct{}{
	"app": {}, "tmp": {}, "logs": {}, "tests": {}, "opt": {}, "usr": {},
	"etc": {}, "proc": {}, "sys": {}, "dev": {}, "var": {}, "bin": {},
	"sbin": {}, "lib": {}, "lib64": {}, "home": {}, "root": {}, "run": {},
	"workspace": {}, "boot": {}, "media": {}, "mnt": {}, "srv": {},
	"lost+found": {},
}

// ValidateWritableDirs accepts at most four safe, absolute top-level paths for
// bounded Harbor scratch tmpfs mounts. The Docker adapter separately limits
// this capability to Harbor verifier sandboxes.
func ValidateWritableDirs(dirs []string) error {
	if len(dirs) > 4 {
		return fmt.Errorf("at most four writable scratch directories are allowed")
	}
	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		if dir == "/" || !strings.HasPrefix(dir, "/") || path.Clean(dir) != dir || path.Dir(dir) != "/" {
			return fmt.Errorf("writable scratch path must be one absolute top-level directory")
		}
		name := strings.TrimPrefix(dir, "/")
		if !writableDirName.MatchString(name) {
			return fmt.Errorf("invalid writable scratch directory name")
		}
		if _, reserved := reservedWritableDirs[strings.ToLower(name)]; reserved {
			return fmt.Errorf("reserved writable scratch directory %q", dir)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate writable scratch directory %q", dir)
		}
		seen[name] = struct{}{}
	}
	return nil
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

// ModeSnapshotter is implemented by sandboxes that can preserve executable
// bits from a snapshot for a later verifier workspace.
type ModeSnapshotter interface {
	SnapshotModes() map[string]uint32
}

type Factory interface {
	Create(context.Context, Spec, Files) (Sandbox, error)
	Doctor(context.Context) (Info, error)
}
