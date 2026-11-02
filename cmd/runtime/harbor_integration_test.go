package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/agent"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/config"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/harbor"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/runner"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	dockersandbox "github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox/docker"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
)

func TestHarborRuntimeEndToEndArtifactsAndFrozenTask(t *testing.T) {
	if os.Getenv("AGENTEVAL_DOCKER_TESTS") != "1" {
		t.Skip("set AGENTEVAL_DOCKER_TESTS=1 to run the Harbor runtime end-to-end test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository root")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
	project := t.TempDir()
	copySandboxHelperBuildInputs(t, repo, project)
	fixture := filepath.Join(repo, "examples", "harbor", "file-artifacts")
	if _, err := os.Stat(filepath.Join(fixture, "environment", "Dockerfile")); err != nil {
		t.Fatalf("expected owned Harbor fixture: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture, "tests", "Dockerfile")); err != nil {
		t.Fatalf("expected fixture verifier Dockerfile: %v", err)
	}

	helperTag := fmt.Sprintf("agenteval-harbor-e2e-helper:%x", time.Now().UnixNano())
	helperImageID, err := dockersandbox.EnsureImage(ctx, project, helperTag)
	if err != nil {
		t.Fatalf("build sandbox helper image from staged trusted inputs: %v", err)
	}
	const taskID = "harbor-file-artifacts-e2e"
	var importStdout, importStderr strings.Builder
	loadedByImport, err := harbor.Import(ctx, harbor.ImportOptions{
		Path: fixture, Project: project, ID: taskID, HelperImageID: helperImageID,
		Stdout: &importStdout, Stderr: &importStderr,
	})
	if err != nil {
		t.Fatalf("import owned Harbor fixture: %v\nstdout=%s\nstderr=%s", err, importStdout.String(), importStderr.String())
	}
	if loadedByImport.Harbor.AgentImage == loadedByImport.Harbor.VerifierImage {
		t.Fatal("import must freeze independent agent and verifier images")
	}
	manifest := filepath.Join(project, "tasks", "harbor", taskID, "task.json")
	loaded, err := task.Load(manifest, project)
	if err != nil {
		t.Fatalf("load imported frozen task: %v", err)
	}
	if loaded.Harbor.AgentImage != loadedByImport.Harbor.AgentImage || loaded.Harbor.VerifierImage != loadedByImport.Harbor.VerifierImage || loaded.Harbor.PackageSHA256 != loadedByImport.Harbor.PackageSHA256 {
		t.Fatal("task.Load changed the frozen Harbor image or package identities")
	}
	if err := os.MkdirAll(filepath.Join(project, "tasks", "fixtures"), 0700); err != nil {
		t.Fatal(err)
	}
	registry, err := newTaskRegistry(project, config.Config{}, helperImageID)
	if err != nil {
		t.Fatalf("create task registry from imported Harbor manifest: %v", err)
	}
	req := jobs.Request{SchemaVersion: 1, TaskID: taskID, Agent: "scripted", Script: "baseline"}
	normalized, frozen, err := registry.prepare(ctx, req)
	if err != nil {
		t.Fatalf("prepare frozen Harbor TaskSnapshot: %v", err)
	}
	manifestBytes, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if normalized != req || frozen.Backend != "harbor" || frozen.PackageSHA256 != loaded.Harbor.PackageSHA256 || frozen.VerifierImageID != loaded.Harbor.VerifierImage || frozen.ImageID != loaded.Harbor.AgentImage || frozen.SourceSHA256 != loaded.SourceSHA256 || frozen.EvaluatorSHA256 != loaded.EvaluatorSHA256 || frozen.ManifestSHA256 != digest(manifestBytes) || frozen.AgentConfigSHA256 == "" {
		t.Fatalf("TaskSnapshot did not freeze package, verifier and agent inputs: request=%+v snapshot=%+v", normalized, frozen)
	}

	factory, err := dockersandbox.New(ctx, dockersandbox.Options{Image: helperImageID})
	if err != nil {
		t.Fatalf("connect Docker factory: %v", err)
	}
	t.Cleanup(func() {
		if err := factory.Close(); err != nil {
			t.Errorf("close Docker factory: %v", err)
		}
	})
	artifactsRoot := filepath.Join(project, "artifacts")
	run := func(name string, a domain.Agent, publicConfig any) runner.Report {
		t.Helper()
		report, runErr := runner.Run(ctx, factory, loaded, a, runner.Options{
			Image: frozen.ImageID, ArtifactsDir: artifactsRoot, AgentName: name, AgentConfig: publicConfig,
		})
		if runErr != nil {
			t.Fatalf("run Harbor %s attempt: %v (reason=%s verdict=%s)", name, runErr, report.Reason, report.Verdict)
		}
		if report.CleanupStatus != "succeeded" || report.Backend != "harbor" || report.PackageSHA256 != frozen.PackageSHA256 || report.VerifierImageID != frozen.VerifierImageID || report.ImageID != frozen.ImageID {
			t.Fatalf("run report differs from frozen Harbor TaskSnapshot or cleanup failed: %+v", report)
		}
		if len(report.SandboxIDs) != 2 || report.SandboxIDs[0] == report.SandboxIDs[1] {
			t.Fatalf("run did not use separate execution and verifier sandboxes: %+v", report.SandboxIDs)
		}
		return report
	}

	baseline := run("scripted", agent.Scripted{Script: loaded.Scripts["baseline"]}, map[string]any{"kind": "scripted", "name": "baseline"})
	if baseline.Verdict != "test_failure" || baseline.Evaluation == nil || baseline.Evaluation.Score == nil || *baseline.Evaluation.Score != 0 || baseline.Evaluation.Classification != "capability_result" {
		t.Fatalf("missing outputs should be a valid zero-score capability result: verdict=%s evaluation=%+v", baseline.Verdict, baseline.Evaluation)
	}
	assertExportedSubmission(t, baseline, filepath.Join(project, "exports", "baseline"), nil)

	positive := run("harbor-test-writer", harborArtifactAgent{}, map[string]any{"kind": "integration-test-agent", "fixture": taskID})
	if positive.Verdict != "success" || positive.Evaluation == nil || positive.Evaluation.Score == nil || *positive.Evaluation.Score != 1 || !positive.Evaluation.PackagePassed {
		t.Fatalf("complete JSON and binary artifacts should score one: verdict=%s evaluation=%+v", positive.Verdict, positive.Evaluation)
	}
	assertExportedSubmission(t, positive, filepath.Join(project, "exports", "positive"), map[string][]byte{
		"answer.json": []byte(`{"value":42}`),
		"answer.bin":  {0x00, 0xff, 0x0a, 0x80},
	})

	if frozen.PackageSHA256 != positive.PackageSHA256 || frozen.VerifierImageID != positive.VerifierImageID {
		t.Fatalf("frozen TaskSnapshot changed across runs: %+v positive report package=%s verifier=%s", frozen, positive.PackageSHA256, positive.VerifierImageID)
	}
	gradePath := filepath.Join(filepath.Dir(manifest), "package", "tests", "grade.py")
	grade, err := os.ReadFile(gradePath)
	if err != nil {
		t.Fatalf("read imported grader for drift check: %v", err)
	}
	if err := os.WriteFile(gradePath, append(grade, []byte("\n# deliberate post-import drift\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := task.Load(manifest, project); err == nil {
		t.Fatal("task.Load accepted post-import verifier/package drift")
	}
	if frozen.PackageSHA256 != loaded.Harbor.PackageSHA256 || frozen.VerifierImageID != loaded.Harbor.VerifierImage {
		t.Fatal("post-import package drift mutated the previously frozen TaskSnapshot")
	}
}

type harborArtifactAgent struct{}

func (harborArtifactAgent) Run(ctx context.Context, box sandbox.Sandbox, task domain.Task, emit domain.Emit) (domain.AgentResult, error) {
	code := `from pathlib import Path; import json; Path("answer.json").write_text(json.dumps({"value": 42}, separators=(",", ":")), encoding="utf-8"); Path("answer.bin").write_bytes(bytes([0, 255, 10, 128]))`
	request := sandbox.ExecRequest{Argv: []string{"python3", "-c", code}, Workdir: task.Workdir(), Timeout: 30 * time.Second, OutputLimit: 64 << 10}
	result, err := box.Exec(ctx, request)
	if err != nil {
		return domain.AgentResult{}, domain.Fail("infra_error", err)
	}
	if result.TimedOut {
		return domain.AgentResult{}, domain.Fail("timeout", context.DeadlineExceeded)
	}
	if result.OOMKilled {
		return domain.AgentResult{}, domain.Fail("resource_limit", fmt.Errorf("artifact writer killed by OOM"))
	}
	if !result.ExitKnown || result.ExitCode != 0 {
		return domain.AgentResult{}, domain.Fail("tool_error", fmt.Errorf("artifact writer failed: exit=%d stderr=%s", result.ExitCode, result.Stderr))
	}
	if emit != nil {
		emit("agent", "test_artifacts_written", map[string]any{"stdout": result.Stdout, "exit_code": result.ExitCode})
	}
	return domain.AgentResult{Reason: "completed", Usage: domain.Usage{Quality: "known"}}, nil
}

func copySandboxHelperBuildInputs(t *testing.T, repo, destination string) {
	t.Helper()
	copyFile := func(relative string) {
		t.Helper()
		source := filepath.Join(repo, filepath.FromSlash(relative))
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read helper build input %s: %v", relative, err)
		}
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			t.Fatalf("stage helper build input %s: %v", relative, err)
		}
	}
	copyFile("go.mod")
	copyFile("deployments/Dockerfile.sandbox")
	if _, err := os.Stat(filepath.Join(repo, ".dockerignore")); err == nil {
		copyFile(".dockerignore")
	}
	entries, err := os.ReadDir(filepath.Join(repo, "cmd", "sandbox-helper"))
	if err != nil {
		t.Fatalf("list helper sources: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		copyFile(filepath.ToSlash(filepath.Join("cmd", "sandbox-helper", entry.Name())))
	}
}

func assertExportedSubmission(t *testing.T, report runner.Report, exportDir string, want map[string][]byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(exportDir), 0700); err != nil {
		t.Fatal(err)
	}
	var out, errout strings.Builder
	if code := executeExport(context.Background(), []string{"--run", report.ArtifactDir, "--output", exportDir}, &out, &errout); code != 0 {
		t.Fatalf("export Harbor run failed: exit=%d stderr=%s", code, errout.String())
	}
	archivePath := filepath.Join(exportDir, "submission.tar.gz")
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("exported Harbor submission missing: %v", err)
	}
	files := readGzipTar(t, archive)
	for name, expected := range want {
		actual, ok := files[name]
		if !ok || !bytes.Equal(actual, expected) {
			t.Fatalf("exported submission %s bytes=%v expected=%v", name, actual, expected)
		}
	}
	if want == nil {
		if _, ok := files["answer.json"]; ok {
			t.Fatal("baseline submission unexpectedly contains answer.json")
		}
		if _, ok := files["answer.bin"]; ok {
			t.Fatal("baseline submission unexpectedly contains answer.bin")
		}
	}
	var reportJSON map[string]any
	runBytes, err := os.ReadFile(filepath.Join(exportDir, "run.json"))
	if err != nil || json.Unmarshal(runBytes, &reportJSON) != nil {
		t.Fatalf("exported run report missing or invalid: %v", err)
	}
	if reportJSON["package_sha256"] != report.PackageSHA256 || reportJSON["verifier_image_id"] != report.VerifierImageID {
		t.Fatalf("export omitted frozen Harbor identities: %v", reportJSON)
	}
}

func readGzipTar(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open submission gzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read submission tar: %v", err)
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > 64<<20 {
			t.Fatalf("invalid submission archive entry: %+v", header)
		}
		contents, err := io.ReadAll(io.LimitReader(tr, header.Size+1))
		if err != nil || int64(len(contents)) != header.Size {
			t.Fatalf("read submission entry %s: bytes=%d err=%v", header.Name, len(contents), err)
		}
		files[header.Name] = contents
	}
	return files
}
