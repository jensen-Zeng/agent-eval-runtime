package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
)

func TestDockerSandboxIntegration(t *testing.T) {
	if os.Getenv("AGENTEVAL_DOCKER_TESTS") != "1" {
		t.Skip("set AGENTEVAL_DOCKER_TESTS=1 to run Docker integration")
	}
	ctx := context.Background()
	f, err := New(ctx, Options{Image: "agenteval-sandbox:v1.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	id := ""
	if imageInfo, _, inspectErr := f.cli.ImageInspectWithRaw(ctx, "agenteval-sandbox:v1.0"); inspectErr == nil {
		id = imageInfo.ID
	} else {
		id, err = f.EnsureImage(ctx, root, "agenteval-sandbox:v1.0")
		if err != nil {
			t.Fatal(err)
		}
	}
	if !strings.HasPrefix(id, "sha256:") {
		t.Fatalf("expected resolved image id, got %q", id)
	}
	sb, err := f.Create(ctx, sandbox.Spec{Image: id, Labels: map[string]string{"agenteval.project": "integration", "agenteval.run": "test", "agenteval.attempt": "1"}, Limits: sandbox.Limits{CPUs: 1, MemoryMiB: 1024, Pids: 64, WorkspaceMiB: 64}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sb.Destroy(cleanup)
	}()
	inst := sb.(*instance)
	ci, err := f.cli.ContainerInspect(ctx, inst.id)
	if err != nil {
		t.Fatal(err)
	}
	if !ci.HostConfig.ReadonlyRootfs || ci.HostConfig.NetworkMode != "none" || ci.HostConfig.Memory != 1024*1024*1024 || ci.HostConfig.MemorySwap != 1024*1024*1024 || ci.HostConfig.PidsLimit == nil || *ci.HostConfig.PidsLimit != 64 {
		t.Fatalf("sandbox constraints missing: %+v", ci.HostConfig)
	}
	if ci.Config.User != "10001:10001" {
		t.Fatalf("unexpected runtime user %q", ci.Config.User)
	}
	if err = sb.WriteFile(ctx, "src/hello.txt", []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	goTestFiles := map[string][]byte{
		"goexec/go.mod":        []byte("module sandbox/probe\n\ngo 1.27.1\n"),
		"goexec/probe.go":      []byte("package probe\nfunc Add(a,b int) int { return a+b }\n"),
		"goexec/probe_test.go": []byte("package probe\nimport \"testing\"\nfunc TestAdd(t *testing.T){ if Add(2,3)!=5 { t.Fatal(\"bad result\") } }\n"),
	}
	for path, data := range goTestFiles {
		if err = sb.WriteFile(ctx, path, data); err != nil {
			t.Fatal(err)
		}
	}
	r, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"go", "test", "./..."}, Workdir: "/workspace/goexec", Timeout: 90 * time.Second})
	if err != nil || !r.ExitKnown || r.ExitCode != 0 {
		t.Fatalf("go test compile/execute result %+v, %v", r, err)
	}
	b, err := sb.ReadFile(ctx, "src/hello.txt")
	if err != nil || string(b) != "hello\n" {
		t.Fatalf("read file = %q, %v", b, err)
	}
	paths, err := sb.ListFiles(ctx, "")
	if err != nil || len(paths) == 0 {
		t.Fatalf("root list = %v, %v", paths, err)
	}
	snapshot, err := sb.Snapshot(ctx)
	if err != nil || string(snapshot["src/hello.txt"]) != "hello\n" {
		t.Fatalf("snapshot = %v, %v", snapshot, err)
	}
	patch := "diff --git a/src/hello.txt b/src/hello.txt\nindex 2cf8d83..4f9f2cab 100644\n--- a/src/hello.txt\n+++ b/src/hello.txt\n@@ -1 +1 @@\n-hello\n+patched\n"
	if err = sb.ApplyPatch(ctx, patch); err != nil {
		t.Fatal(err)
	}
	if err = sb.ApplyPatch(ctx, "diff --git a/../../tmp/x b/../../tmp/x\n--- a/../../tmp/x\n+++ b/../../tmp/x\n@@ -1 +1 @@\n-a\n+b\n"); err == nil {
		t.Fatal("accepted escaping patch")
	}
	r, err = sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/usr/local/bin/sandbox-helper", "--oom-probe"}, Timeout: 60 * time.Second})
	if err != nil || !r.OOMKilled {
		t.Fatalf("OOM recognition result %+v, %v", r, err)
	}
	r, err = sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/sh", "-c", "printf ok"}, Timeout: 5 * time.Second})
	if err != nil || !r.ExitKnown || r.ExitCode != 0 || r.Stdout != "ok" {
		t.Fatalf("exec result %+v, %v", r, err)
	}
	r, err = sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/sh", "-c", "head -c 200000 /dev/zero | tr '\\0' x"}, Timeout: 5 * time.Second})
	if err != nil || !r.Truncated || len(r.Stdout) != 64<<10 {
		t.Fatalf("output bound result bytes=%d truncated=%v err=%v", len(r.Stdout), r.Truncated, err)
	}
	r, err = sb.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/sh", "-c", "sleep 10"}, Timeout: 250 * time.Millisecond})
	if err != nil || !r.TimedOut {
		t.Fatalf("timeout result %+v, %v", r, err)
	}
	ci, err = f.cli.ContainerInspect(ctx, inst.id)
	if err != nil {
		t.Fatal(err)
	}
	if ci.State.Running {
		t.Fatal("container remained running after timeout")
	}
	if err = sb.Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	if err = sb.Destroy(ctx); err != nil {
		t.Fatalf("second destroy: %v", err)
	}
}
