package docker

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"io"
	"time"
)

// Stream directly into the helper running in the mounted workspace. Docker's
// archive copy API does not reliably target a container's running tmpfs mounts.
func (s *instance) execStreamInput(ctx context.Context, argv []string, input io.Reader) (sandbox.ExecResult, error) {
	created, err := s.cli.ContainerExecCreate(ctx, s.id, types.ExecConfig{Cmd: argv, Env: s.helperEnv(), AttachStdin: true, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	a, err := s.cli.ContainerExecAttach(ctx, created.ID, types.ExecStartCheck{})
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	defer a.Close()
	out, stderr := &bounded{limit: streamLimit}, &bounded{limit: streamLimit}
	done := make(chan error, 2)
	go func() { _, err := io.Copy(a.Conn, input); _ = a.CloseWrite(); done <- err }()
	go func() { _, err := stdcopy.StdCopy(out, stderr, a.Reader); done <- err }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				return sandbox.ExecResult{}, err
			}
		case <-ctx.Done():
			a.Close()
			return sandbox.ExecResult{}, ctx.Err()
		}
	}
	check, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	x, err := s.cli.ContainerExecInspect(check, created.ID)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	if out.truncated || stderr.truncated {
		return sandbox.ExecResult{}, fmt.Errorf("seed helper output limit")
	}
	return sandbox.ExecResult{Stdout: out.b.String(), Stderr: stderr.b.String(), ExitCode: x.ExitCode, ExitKnown: !x.Running}, nil
}
