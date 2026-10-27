package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
)

func makeImportFixture(t *testing.T) (project, source, grader, prompt string) {
	t.Helper()
	base := t.TempDir()
	project = filepath.Join(base, "trusted")
	for _, dir := range []string{filepath.Join(project, "tasks", "fixtures"), filepath.Join(base, "source"), filepath.Join(base, "grader")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	source = filepath.Join(base, "source")
	grader = filepath.Join(base, "grader")
	prompt = filepath.Join(base, "prompt.txt")
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(name, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(source, "go.mod"), "module example.test/project\n\ngo 1.27.1\n")
	write(filepath.Join(source, "main.go"), "package main\n\nfunc Answer() int { return 42 }\n")
	write(filepath.Join(grader, "grade.py"), "# trusted grader fixture\n")
	write(filepath.Join(grader, "private_test.go"), "package main\n")
	write(prompt, "请实现 Answer 函数并保持接口不变。\n")
	return
}

func invokeImport(project, id, source, prompt, grader string, out, errout *strings.Builder) int {
	return executeImport(context.Background(), []string{
		"--project", project, "--id", id, "--source", source,
		"--prompt", prompt, "--grader-dir", grader,
	}, out, errout)
}

func TestExecuteImportCreatesValidatedIsolatedTask(t *testing.T) {
	project, source, grader, prompt := makeImportFixture(t)
	var out, errout strings.Builder
	if code := invokeImport(project, "fixture-one", source, prompt, grader, &out, &errout); code != 0 {
		t.Fatalf("import exit=%d stderr=%s", code, errout.String())
	}
	destination := filepath.Join(project, "tasks", "fixtures", "fixture-one")
	loaded, err := task.Load(filepath.Join(destination, "task.json"), project)
	if err != nil {
		t.Fatalf("task.Load: %v", err)
	}
	if loaded.ID != "fixture-one" || loaded.Grader == nil || loaded.Grader.Kind != "python" {
		t.Fatalf("unexpected imported task: %#v", loaded)
	}
	if loaded.Prompt != "请实现 Answer 函数并保持接口不变。\n" {
		t.Fatalf("UTF-8 prompt changed: %q", loaded.Prompt)
	}
	if _, err := os.Stat(filepath.Join(destination, "source", "grade.py")); !os.IsNotExist(err) {
		t.Fatalf("grader leaked into source bundle, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "source", "private_test.go")); !os.IsNotExist(err) {
		t.Fatalf("private tests leaked into source bundle, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "evaluator", "grade.py")); err != nil {
		t.Fatalf("trusted grader missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "evaluator", "private_test.go")); err != nil {
		t.Fatalf("trusted tests missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "go.mod")); err != nil {
		t.Fatalf("nested module marker missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "go.mod")); !os.IsNotExist(err) {
		t.Fatalf("import wrote/changed trusted project root go.mod, err=%v", err)
	}
}

func TestExecuteImportRejectsDuplicateWithoutChangingDestination(t *testing.T) {
	project, source, grader, prompt := makeImportFixture(t)
	destination := filepath.Join(project, "tasks", "fixtures", "occupied")
	if err := os.Mkdir(destination, 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(destination, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep existing data"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errout strings.Builder
	if code := invokeImport(project, "occupied", source, prompt, grader, &out, &errout); code == 0 {
		t.Fatal("duplicate task import unexpectedly succeeded")
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "keep existing data" {
		t.Fatalf("existing destination changed: contents=%q err=%v", got, err)
	}
}

func TestExecuteImportRejectsInvalidUTF8PromptBeforeWrites(t *testing.T) {
	project, source, grader, prompt := makeImportFixture(t)
	if err := os.WriteFile(prompt, []byte{0xff, 0xfe, 'x'}, 0600); err != nil {
		t.Fatal(err)
	}
	var out, errout strings.Builder
	if code := invokeImport(project, "bad-prompt", source, prompt, grader, &out, &errout); code == 0 {
		t.Fatal("invalid UTF-8 prompt unexpectedly succeeded")
	}
	destination := filepath.Join(project, "tasks", "fixtures", "bad-prompt")
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("invalid prompt created destination, err=%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(project, "tasks", "fixtures"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid prompt left staged writes: %v", entries)
	}
}
