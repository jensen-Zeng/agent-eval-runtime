package main

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/dataset"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatasetSelectionRequiresVersionChoice(t *testing.T) {
	c := dataset.Catalog{Items: []dataset.Item{{Key: "first", TaskID: "same"}, {Key: "second", TaskID: "same"}}}
	if _, err := selectDatasetItems(c, "same", ""); err == nil {
		t.Fatal("ambiguous upstream task selected")
	}
	if _, err := selectDatasetItems(c, "first,second", ""); err == nil {
		t.Fatal("colliding destination task IDs accepted")
	}
	items, err := selectDatasetItems(c, "second", "custom-")
	if err != nil || len(items) != 1 || items[0].Key != "second" {
		t.Fatal(items, err)
	}
}

func TestDatasetGenericImportDryRunAndVerifiedResume(t *testing.T) {
	project, source, grader, prompt := makeImportFixture(t)
	base := filepath.Dir(source)
	rel := func(s string) string {
		p, err := filepath.Rel(base, s)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.ToSlash(p)
	}
	c := dataset.Catalog{SchemaVersion: 1, Name: "custom-corpus", Root: base, Adapter: "manifest", Items: []dataset.Item{{Key: "custom-one", TaskID: "answer-one", Source: rel(source), GraderDir: rel(grader), GraderEntry: "grade.py", PromptFiles: []string{rel(prompt)}, State: "candidate"}}}
	file := filepath.Join(base, "catalog.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"import", "--file", file, "--project", project, "--select", "custom-one"}
	var out, errout strings.Builder
	if code := executeDataset(context.Background(), append(append([]string{}, args...), "--dry-run"), &out, &errout); code != 0 {
		t.Fatalf("dry exit %d: %s", code, errout.String())
	}
	dest := filepath.Join(project, "tasks", "fixtures", "answer-one")
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("dry run registered task", err)
	}
	out.Reset()
	errout.Reset()
	if code := executeDataset(context.Background(), args, &out, &errout); code != 0 {
		t.Fatalf("import exit %d: %s", code, errout.String())
	}
	var first datasetImportReport
	if err := json.Unmarshal([]byte(out.String()), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Results) != 1 || first.Results[0].Status != "imported" || !first.RestartRequired {
		t.Fatal(first)
	}
	report := filepath.Join(base, "import.json")
	b, _ = json.Marshal(first)
	os.WriteFile(report, b, 0600)
	out.Reset()
	errout.Reset()
	if code := executeDataset(context.Background(), append(append([]string{}, args...), "--if-exists", "verify"), &out, &errout); code != 0 {
		t.Fatalf("verify exit %d: %s", code, errout.String())
	}
	if !strings.Contains(out.String(), "verified_existing") {
		t.Fatal(out.String())
	}
	out.Reset()
	errout.Reset()
	matrix := filepath.Join(base, "matrix.json")
	if code := executeDataset(context.Background(), []string{"matrix", "--report", report, "--output", matrix, "--models", "model-a,model-b", "--repeats", "4"}, &out, &errout); code != 0 {
		t.Fatal(code, errout.String())
	}
	input, _, err := readExperiment(matrix)
	if err != nil || len(input.Tasks) != 1 || len(input.Variants) != 2 || input.Repeats != 4 {
		t.Fatal(input, err)
	}
	os.WriteFile(filepath.Join(source, "main.go"), []byte("package main\nfunc Answer() int {return 99}\n"), 0600)
	out.Reset()
	errout.Reset()
	if code := executeDataset(context.Background(), append(append([]string{}, args...), "--if-exists", "verify"), &out, &errout); code != 2 {
		t.Fatalf("changed source accepted %d %s %s", code, out.String(), errout.String())
	}
}

func TestDatasetMatrixRejectsDryRunAndExcessiveJobs(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "report.json")
	report := datasetImportReport{SchemaVersion: 1, DryRun: true, Results: []datasetImportResult{{TaskID: "one", Status: "validated"}}}
	b, _ := json.Marshal(report)
	os.WriteFile(file, b, 0600)
	var out, errout strings.Builder
	if code := executeDatasetMatrix([]string{"--report", file, "--output", filepath.Join(dir, "matrix.json")}, &out, &errout); code == 0 {
		t.Fatal("dry-run generated executable matrix")
	}
}

func TestDatasetImageContainsOnlyModuleInputs(t *testing.T) {
	_, source, grader, prompt := makeImportFixture(t)
	base := filepath.Dir(source)
	c := dataset.Catalog{SchemaVersion: 1, Name: "generic", Root: base, Adapter: "manifest", Items: []dataset.Item{{Key: "one", TaskID: "one", Source: "source", GraderDir: filepath.Base(grader), GraderEntry: "grade.py", PromptFiles: []string{filepath.Base(prompt)}, State: "candidate"}}}
	b, _ := json.Marshal(c)
	file := filepath.Join(base, "catalog.json")
	os.WriteFile(file, b, 0600)
	dest := filepath.Join(base, "image")
	var out, errout strings.Builder
	if code := executeDatasetImage(context.Background(), []string{"--file", file, "--select", "one", "--output", dest}, &out, &errout); code != 0 {
		t.Fatal(code, errout.String())
	}
	if _, err := os.Stat(filepath.Join(dest, "modules", "one", "go.mod")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"main.go", "grade.py", "private_test.go"} {
		if _, err := os.Stat(filepath.Join(dest, "modules", "one", p)); !os.IsNotExist(err) {
			t.Fatalf("non-module input included: %s", p)
		}
	}
	out.Reset()
	errout.Reset()
	if code := executeDatasetImage(context.Background(), []string{"--file", file, "--select", "one", "--output", dest}, &out, &errout); code == 0 {
		t.Fatal("image context overwritten")
	}
}
