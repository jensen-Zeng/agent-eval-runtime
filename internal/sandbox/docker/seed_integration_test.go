package docker

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
)

func TestDockerSeedArchiveRoundTrip(t *testing.T) {
	if os.Getenv("AGENTEVAL_DOCKER_TESTS") != "1" {
		t.Skip("set AGENTEVAL_DOCKER_TESTS=1 to run Docker seed integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	factory, err := New(ctx, Options{Image: "agenteval-sandbox:v1.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer factory.Close()
	info, err := factory.Doctor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.OSType != "linux" || (info.Architecture != "amd64" && info.Architecture != "x86_64") || info.ImageID == "" {
		t.Fatalf("Docker must have the prepared Linux amd64 v1.0 image: %+v", info)
	}
	files := sandbox.Files{
		".hidden":              []byte("hidden-root\n"),
		"nested/.secret":       []byte("nested-hidden\n"),
		"nested/src/main.go":   []byte("package main\nfunc main() {}\n"),
		"nested/src/README.md": []byte("seeded by one archive\n"),
	}
	container, err := factory.Create(ctx, sandbox.Spec{
		Image: info.ImageID,
		Labels: map[string]string{
			"agenteval.project": "seed-integration",
			"agenteval.run":     "seed-roundtrip",
			"agenteval.attempt": "seed-roundtrip-1",
		},
		Limits: sandbox.Limits{CPUs: 1, MemoryMiB: 1024, Pids: 64, WorkspaceMiB: 64},
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := container.Destroy(cleanup); err != nil {
			t.Errorf("destroy test-owned sandbox: %v", err)
		}
	}()

	snapshot, err := container.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != len(files) {
		t.Fatalf("snapshot contains %d files, expected %d: %v", len(snapshot), len(files), snapshot)
	}
	for path, expected := range files {
		actual, ok := snapshot[path]
		if !ok || !bytes.Equal(actual, expected) {
			t.Errorf("snapshot[%q]=%q present=%t, want %q", path, actual, ok, expected)
		}
		read, err := container.ReadFile(ctx, path)
		if err != nil || !bytes.Equal(read, expected) {
			t.Errorf("ReadFile(%q)=%q err=%v, want %q", path, read, err, expected)
		}
	}
	stat, err := container.Exec(ctx, sandbox.ExecRequest{Argv: []string{"stat", "-c", "%u:%g:%a", "/workspace/.hidden", "/workspace/nested/src/main.go"}, Workdir: "/workspace", Timeout: 10 * time.Second, OutputLimit: 64 << 10})
	if err != nil || !stat.ExitKnown || stat.ExitCode != 0 {
		t.Fatalf("stat seeded file ownership/modes: result=%+v err=%v", stat, err)
	}
	if want := "10001:10001:644\n10001:10001:644\n"; stat.Stdout != want {
		t.Fatalf("seeded file ownership/modes=%q want=%q", stat.Stdout, want)
	}
}
