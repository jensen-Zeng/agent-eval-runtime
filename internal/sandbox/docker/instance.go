package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const streamLimit = 64 << 10
const helperOutputLimit = 72 << 20

type instance struct {
	cli       *client.Client
	id        string
	mu        sync.Mutex
	destroyed bool
}

func (s *instance) ID() string { return s.id }

type bounded struct {
	b         bytes.Buffer
	limit     int
	truncated bool
}

func (w *bounded) Write(p []byte) (int, error) {
	n := len(p)
	remain := w.limit - w.b.Len()
	if remain > 0 {
		if remain > n {
			remain = n
		}
		_, _ = w.b.Write(p[:remain])
	}
	if remain < n {
		w.truncated = true
	}
	return n, nil
}

func (s *instance) Exec(ctx context.Context, q sandbox.ExecRequest) (sandbox.ExecResult, error) {
	start := time.Now()
	if len(q.Argv) == 0 {
		return sandbox.ExecResult{}, fmt.Errorf("argv is empty")
	}
	baselineCtx, baselineCancel := context.WithTimeout(ctx, 5*time.Second)
	baselineOOM, baselineErr := s.readOOMKillCount(baselineCtx)
	baselineCancel()
	if baselineErr != nil {
		return sandbox.ExecResult{}, fmt.Errorf("verify cgroup OOM event counter: %w", baselineErr)
	}
	timeout := q.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	work := q.Workdir
	if work == "" {
		work = "/workspace"
	}
	lim := q.OutputLimit
	if lim <= 0 || lim > streamLimit {
		lim = streamLimit
	}
	out, stderr := &bounded{limit: lim}, &bounded{limit: lim}
	created, err := s.cli.ContainerExecCreate(runCtx, s.id, types.ExecConfig{Cmd: q.Argv, Env: q.Env, WorkingDir: work, AttachStdout: true, AttachStderr: true, Tty: false})
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	attached, err := s.cli.ContainerExecAttach(runCtx, created.ID, types.ExecStartCheck{Tty: false})
	if err != nil {
		if runCtx.Err() != nil {
			_ = s.stopContainer()
		}
		return sandbox.ExecResult{}, err
	}
	defer attached.Close()
	drainDone := make(chan error, 1)
	go func() { _, e := stdcopy.StdCopy(out, stderr, attached.Reader); drainDone <- e }()
	var drainErr error
	select {
	case drainErr = <-drainDone:
	case <-runCtx.Done():
		stopErr := s.stopContainer()
		attached.Close()
		select {
		case drainErr = <-drainDone:
		case <-time.After(2 * time.Second):
			return sandbox.ExecResult{}, fmt.Errorf("output stream did not close after stopping container: %w", stopErr)
		}
		if stopErr != nil {
			return sandbox.ExecResult{}, fmt.Errorf("stop container after command cancellation: %w", stopErr)
		}
	}
	inspectCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	ex, inspectErr := s.cli.ContainerExecInspect(inspectCtx, created.ID)
	state, stateErr := s.cli.ContainerInspect(inspectCtx, s.id)
	res := sandbox.ExecResult{Stdout: out.b.String(), Stderr: stderr.b.String(), Truncated: out.truncated || stderr.truncated, DurationMS: time.Since(start).Milliseconds()}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		res.Canceled = true
	}
	if inspectErr == nil {
		res.ExitCode = ex.ExitCode
		res.ExitKnown = ex.Running == false
	}
	if stateErr == nil && state.State != nil {
		res.OOMKilled = state.State.OOMKilled
	}
	if !res.TimedOut && !res.Canceled {
		afterCtx, afterCancel := context.WithTimeout(context.Background(), 3*time.Second)
		after, e := s.readOOMKillCount(afterCtx)
		afterCancel()
		if e == nil && after > baselineOOM {
			res.OOMKilled = true
		}
		if e != nil && !res.OOMKilled {
			return res, fmt.Errorf("read cgroup OOM event counter after command: %w", e)
		}
	}
	if res.TimedOut || res.Canceled {
		if stateErr == nil && state.State != nil && state.State.Running {
			return res, fmt.Errorf("container still running after kill")
		}
		return res, nil
	}
	if drainErr != nil && !errors.Is(drainErr, io.EOF) {
		return res, fmt.Errorf("read command output: %w", drainErr)
	}
	if inspectErr != nil {
		return res, fmt.Errorf("inspect exec: %w", inspectErr)
	}
	return res, nil
}

func (s *instance) stopContainer() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	killErr := s.cli.ContainerKill(ctx, s.id, "KILL")
	if killErr != nil && !client.IsErrNotFound(killErr) {
		in, e := s.cli.ContainerInspect(ctx, s.id)
		if e != nil {
			return fmt.Errorf("kill container: %w (inspect: %v)", killErr, e)
		}
		if in.State == nil || in.State.Running {
			return fmt.Errorf("kill container: %w", killErr)
		}
	}
	waitC, errC := s.cli.ContainerWait(ctx, s.id, container.WaitConditionNotRunning)
	select {
	case <-waitC:
	case e := <-errC:
		if e != nil && !client.IsErrNotFound(e) {
			return e
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	in, err := s.cli.ContainerInspect(ctx, s.id)
	if err != nil {
		if client.IsErrNotFound(err) {
			return nil
		}
		return err
	}
	if in.State == nil || in.State.Running {
		return fmt.Errorf("container remains running")
	}
	return nil
}

type helperRequest struct {
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	Data  []byte `json:"data,omitempty"`
	Patch string `json:"patch,omitempty"`
}
type helperResponse struct {
	OK    bool              `json:"ok"`
	Data  []byte            `json:"data,omitempty"`
	Paths []string          `json:"paths,omitempty"`
	Files map[string][]byte `json:"files,omitempty"`
	Error string            `json:"error,omitempty"`
}

func (s *instance) helper(ctx context.Context, q helperRequest) (helperResponse, error) {
	b, _ := json.Marshal(q)
	created, err := s.cli.ContainerExecCreate(ctx, s.id, types.ExecConfig{Cmd: []string{"/usr/local/bin/sandbox-helper"}, AttachStdin: true, AttachStdout: true, AttachStderr: true, Tty: false})
	if err != nil {
		return helperResponse{}, err
	}
	a, err := s.cli.ContainerExecAttach(ctx, created.ID, types.ExecStartCheck{Tty: false})
	if err != nil {
		if ctx.Err() != nil {
			_ = s.stopContainer()
		}
		return helperResponse{}, err
	}
	closed := make(chan struct{})
	watchDone := make(chan error, 1)
	var closeOnce sync.Once
	stopWatch := func() { closeOnce.Do(func() { close(closed) }) }
	go func() {
		select {
		case <-ctx.Done():
			a.Close()
			watchDone <- s.stopContainer()
		case <-closed:
			watchDone <- nil
		}
	}()
	defer func() { stopWatch(); a.Close() }()
	if _, err = a.Conn.Write(append(b, '\n')); err != nil {
		return helperResponse{}, err
	}
	_ = a.CloseWrite()
	stdout := &bounded{limit: helperOutputLimit}
	stderr := &bounded{limit: streamLimit}
	_, err = stdcopy.StdCopy(stdout, stderr, a.Reader)
	if ctx.Err() != nil {
		stopErr := <-watchDone
		if stopErr != nil {
			return helperResponse{}, fmt.Errorf("stop container after helper cancellation: %w", stopErr)
		}
		return helperResponse{}, ctx.Err()
	}
	stopWatch()
	a.Close()
	<-watchDone
	if err != nil {
		return helperResponse{}, err
	}
	var hr helperResponse
	if stdout.truncated {
		return helperResponse{}, fmt.Errorf("sandbox helper response exceeds %d bytes", helperOutputLimit)
	}
	if e := json.Unmarshal(stdout.b.Bytes(), &hr); e != nil {
		return hr, fmt.Errorf("parse sandbox helper response: %w (%q)", e, string(stdout.b.Bytes()[:min(stdout.b.Len(), 256)]))
	}
	if !hr.OK {
		return hr, errors.New(hr.Error)
	}
	return hr, nil
}
func (s *instance) ListFiles(ctx context.Context, p string) ([]string, error) {
	r, e := s.helper(ctx, helperRequest{Op: "list", Path: p})
	return r.Paths, e
}
func (s *instance) ReadFile(ctx context.Context, p string) ([]byte, error) {
	r, e := s.helper(ctx, helperRequest{Op: "read", Path: p})
	return r.Data, e
}
func (s *instance) WriteFile(ctx context.Context, p string, b []byte) error {
	_, e := s.helper(ctx, helperRequest{Op: "write", Path: p, Data: b})
	return e
}
func (s *instance) ApplyPatch(ctx context.Context, patch string) error {
	if _, e := s.helper(ctx, helperRequest{Op: "patch", Patch: patch}); e != nil {
		return e
	}
	r, e := s.execInput(ctx, []string{"git", "apply", "--whitespace=nowarn", "-"}, []byte(patch))
	if e != nil {
		return e
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("git apply failed: %s", strings.TrimSpace(r.Stderr))
	}
	return nil
}
func (s *instance) execInput(ctx context.Context, argv []string, input []byte) (sandbox.ExecResult, error) {
	created, e := s.cli.ContainerExecCreate(ctx, s.id, types.ExecConfig{Cmd: argv, AttachStdin: true, AttachStdout: true, AttachStderr: true, Tty: false})
	if e != nil {
		return sandbox.ExecResult{}, e
	}
	a, e := s.cli.ContainerExecAttach(ctx, created.ID, types.ExecStartCheck{Tty: false})
	if e != nil {
		return sandbox.ExecResult{}, e
	}
	defer a.Close()
	_, e = a.Conn.Write(input)
	if e != nil {
		return sandbox.ExecResult{}, e
	}
	_ = a.CloseWrite()
	out, stderr := &bounded{limit: streamLimit}, &bounded{limit: streamLimit}
	_, e = stdcopy.StdCopy(out, stderr, a.Reader)
	if e != nil {
		return sandbox.ExecResult{}, e
	}
	x, e := s.cli.ContainerExecInspect(ctx, created.ID)
	return sandbox.ExecResult{ExitCode: x.ExitCode, ExitKnown: !x.Running, Stdout: out.b.String(), Stderr: stderr.b.String()}, e
}

func (s *instance) readOOMKillCount(ctx context.Context) (uint64, error) {
	r, err := s.execInput(ctx, []string{"/bin/sh", "-c", "cat /sys/fs/cgroup/memory.events"}, nil)
	if err != nil {
		return 0, err
	}
	if r.ExitCode != 0 {
		return 0, fmt.Errorf("read cgroup memory.events: %s", strings.TrimSpace(r.Stderr))
	}
	for _, line := range strings.Split(r.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "oom_kill" {
			var n uint64
			if _, err = fmt.Sscan(fields[1], &n); err != nil {
				return 0, err
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("memory.events lacks oom_kill counter")
}
func (s *instance) Snapshot(ctx context.Context) (sandbox.Files, error) {
	r, e := s.helper(ctx, helperRequest{Op: "snapshot"})
	return sandbox.Files(r.Files), e
}
func (s *instance) Stats(ctx context.Context) (sandbox.Stats, error) {
	resp, e := s.cli.ContainerStatsOneShot(ctx, s.id)
	if e != nil {
		return sandbox.Stats{}, e
	}
	defer resp.Body.Close()
	var d struct {
		Memory struct {
			Usage uint64 `json:"usage"`
		} `json:"memory_stats"`
		CPU struct {
			Usage struct {
				Total uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
	}
	if e = json.NewDecoder(resp.Body).Decode(&d); e != nil {
		return sandbox.Stats{}, e
	}
	in, e := s.cli.ContainerInspect(ctx, s.id)
	oom := e == nil && in.State != nil && in.State.OOMKilled
	cpu := float64(d.CPU.Usage.Total) / 1e9
	return sandbox.Stats{CPUSeconds: cpu, MemoryBytes: d.Memory.Usage, OOMKilled: oom}, nil
}
func (s *instance) Destroy(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return nil
	}
	killErr := s.cli.ContainerKill(ctx, s.id, "KILL")
	if killErr != nil && !client.IsErrNotFound(killErr) {
		inspect, e := s.cli.ContainerInspect(ctx, s.id)
		if e != nil && client.IsErrNotFound(e) {
			s.destroyed = true
			return nil
		}
		if e == nil && inspect.State != nil && !inspect.State.Running {
			killErr = nil
		} else {
			return killErr
		}
	}
	waitC, errC := s.cli.ContainerWait(ctx, s.id, container.WaitConditionNotRunning)
	select {
	case <-waitC:
	case e := <-errC:
		if e != nil && !client.IsErrNotFound(e) {
			return e
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	removeErr := s.cli.ContainerRemove(ctx, s.id, container.RemoveOptions{Force: true})
	if client.IsErrNotFound(removeErr) {
		removeErr = nil
	}
	if removeErr == nil {
		s.destroyed = true
	}
	return removeErr
}
