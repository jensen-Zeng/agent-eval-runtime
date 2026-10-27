package main

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/config"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/runner"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/scheduler"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type v10PreviewStore struct{ jobs.Store }

func (v10PreviewStore) OwnerID() string              { return "0123456789abcdef01234567" }
func (v10PreviewStore) Health(context.Context) error { return nil }

func TestSchedulerPreviewStripsClientExpectedSnapshot(t *testing.T) {
	store := v10PreviewStore{}
	want := jobs.TaskSnapshot{TaskRevision: 1, ManifestSHA256: strings.Repeat("a", 64), SourceSHA256: strings.Repeat("b", 64), EvaluatorSHA256: strings.Repeat("c", 64), ImageID: "sha256:" + strings.Repeat("d", 64), AgentConfigSHA256: strings.Repeat("e", 64)}
	called := false
	svc, err := scheduler.New(scheduler.Options{
		Store: store, Workers: 1, QueueCapacity: 1, ArtifactsDir: filepath.Join(t.TempDir(), "artifacts"),
		Prepare: func(_ context.Context, req jobs.Request) (jobs.Request, jobs.TaskSnapshot, error) {
			called = true
			if req.ExpectedSnapshot != nil {
				t.Fatal("untrusted expected snapshot reached task preparation")
			}
			return req, want, nil
		},
		Execute: func(context.Context, jobs.Job, func(string, string) error) (runner.Report, error) {
			return runner.Report{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := jobs.Request{SchemaVersion: 1, TaskID: "task-a", Agent: "scripted", Script: "success", ExpectedSnapshot: &jobs.TaskSnapshot{TaskRevision: 99}}
	preview, err := svc.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("task preview callback was not called")
	}
	if preview.Request.ExpectedSnapshot != nil {
		t.Fatal("server returned the client-controlled expected snapshot")
	}
	if preview.TaskSnapshot != want {
		t.Fatalf("preview changed trusted snapshot: %+v", preview.TaskSnapshot)
	}
}

func TestCachedPrepareIsStableAndWorkerGuardDetectsManifestDrift(t *testing.T) {
	root := copyRegistryFixture(t)
	imageID := "sha256:" + strings.Repeat("f", 64)
	reg, err := newTaskRegistry(root, config.Default(), imageID)
	if err != nil {
		t.Fatal(err)
	}
	request := jobs.Request{SchemaVersion: 1, TaskID: "go-parser-001", Agent: "scripted", Script: "success"}
	_, frozen, err := reg.prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	manifest := reg.manifests[request.TaskID]
	contents, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, append(contents, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	_, cached, err := reg.prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if cached != frozen {
		t.Fatalf("cached task snapshot changed: before=%+v after=%+v", frozen, cached)
	}
	fresh, err := task.Load(manifest, root)
	if err != nil {
		t.Fatalf("whitespace-only manifest drift should remain loadable: %v", err)
	}
	current, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	workerDetectsDrift := digest(current) != frozen.ManifestSHA256 || fresh.SourceSHA256 != frozen.SourceSHA256 || fresh.EvaluatorSHA256 != frozen.EvaluatorSHA256
	if !workerDetectsDrift {
		t.Fatal("worker snapshot checks would miss post-preview edits")
	}
}

func copyRegistryFixture(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	project := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	root := t.TempDir()
	copyTree := func(src, rel string) {
		t.Helper()
		err := filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return os.ErrInvalid
			}
			target := filepath.Join(root, rel)
			if path != src {
				r, e := filepath.Rel(src, path)
				if e != nil {
					return e
				}
				target = filepath.Join(target, r)
			}
			if d.IsDir() {
				return os.MkdirAll(target, 0700)
			}
			if !info.Mode().IsRegular() {
				return os.ErrInvalid
			}
			data, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			return os.WriteFile(target, data, 0600)
		})
		if err != nil {
			t.Fatalf("copy fixture %s: %v", rel, err)
		}
	}
	copyTree(filepath.Join(project, "tasks", "fixtures", "go-parser-001"), filepath.Join("tasks", "fixtures", "go-parser-001"))
	copyTree(filepath.Join(project, "evaluators", "go-parser-001"), filepath.Join("evaluators", "go-parser-001"))
	return root
}
