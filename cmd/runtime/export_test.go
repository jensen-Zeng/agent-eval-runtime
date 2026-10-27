package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/runner"
)

func exportFixture(t *testing.T, status string) (string, runner.Report, []byte, []byte) {
	t.Helper()
	root := t.TempDir()
	runID := "0123456789abcdef01234567"
	attempt := runID + "-1"
	attemptDir := filepath.Join(root, "attempts", attempt)
	if err := os.MkdirAll(attemptDir, 0700); err != nil {
		t.Fatal(err)
	}
	patch := []byte("diff --git a/main.go b/main.go\n")
	trace := []byte("{\"sequence\":1,\"type\":\"run_started\"}\n{\"sequence\":2,\"type\":\"run_finished\"}\n")
	if err := os.WriteFile(filepath.Join(attemptDir, "candidate.patch"), patch, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attemptDir, "trace.jsonl"), trace, 0600); err != nil {
		t.Fatal(err)
	}
	ph := sha256.Sum256(patch)
	report := runner.Report{
		SchemaVersion: 1, Version: runner.Version, RunID: runID, AttemptID: attempt,
		TaskID: "fixture-task", TaskRevision: 1, Status: status, Verdict: "FAILED", Reason: "fixture result",
		StartedAt: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 10, 6, 0, 0, 1, 0, time.UTC),
		PatchSHA256: hex.EncodeToString(ph[:]), Agent: "scripted", AgentConfigSHA256: strings.Repeat("a", 64),
		ArtifactDir: root, Artifacts: map[string]string{"patch": filepath.ToSlash(filepath.Join("attempts", attempt, "candidate.patch")), "trace": filepath.ToSlash(filepath.Join("attempts", attempt, "trace.jsonl"))},
	}
	result, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "result.json"), result, 0600); err != nil {
		t.Fatal(err)
	}
	return root, report, patch, trace
}

func invokeExport(runDir, output string, out, errout *strings.Builder) int {
	return executeExport(context.Background(), []string{"--run", runDir, "--output", output}, out, errout)
}

func TestExecuteExportWritesHashManifestAndPreservesEvidence(t *testing.T) {
	root, report, patch, trace := exportFixture(t, "SUCCEEDED")
	evaluation := []byte("{\"verdict\":\"PASS\"}\n")
	evalRel := filepath.ToSlash(filepath.Join("attempts", report.AttemptID, "evaluation.json"))
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(evalRel)), evaluation, 0600); err != nil {
		t.Fatal(err)
	}
	report.Artifacts["evaluation"] = evalRel
	rewriteExportReport(t, root, report)
	if err := os.WriteFile(filepath.Join(root, "provenance.json"), []byte("{\"source\":\"fixture\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "bundle")
	var out, errout strings.Builder
	if code := invokeExport(root, outDir, &out, &errout); code != 0 {
		t.Fatalf("export exit=%d stderr=%s", code, errout.String())
	}
	for name, want := range map[string][]byte{"candidate.diff": patch, "trajectory.jsonl": trace, "run.json": mustRead(t, filepath.Join(root, "result.json")), "provenance.json": []byte("{\"source\":\"fixture\"}\n")} {
		got, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s bytes changed", name)
		}
	}
	if got, err := os.ReadFile(filepath.Join(outDir, "evaluation.json")); err != nil || string(got) != string(evaluation) {
		t.Fatalf("evaluation evidence changed or missing: %q err=%v", got, err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(outDir, "BUNDLE_MANIFEST.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		RunID     string `json:"run_id"`
		ResultSHA string `json:"result_sha256"`
		Files     map[string]struct {
			SHA    string `json:"sha256"`
			Size   int    `json:"size_bytes"`
			Source string `json:"source"`
		} `json:"files"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.RunID != report.RunID {
		t.Fatalf("manifest run id=%q", manifest.RunID)
	}
	result := mustRead(t, filepath.Join(root, "result.json"))
	rh := sha256.Sum256(result)
	if manifest.ResultSHA != hex.EncodeToString(rh[:]) {
		t.Fatal("manifest result hash mismatch")
	}
	if len(manifest.Files) != 7 {
		t.Fatalf("manifest file count=%d", len(manifest.Files))
	}
	for name, entry := range manifest.Files {
		data := mustRead(t, filepath.Join(outDir, name))
		h := sha256.Sum256(data)
		if entry.SHA != hex.EncodeToString(h[:]) || entry.Size != len(data) || entry.Source == "" {
			t.Fatalf("invalid manifest record for %s: %+v", name, entry)
		}
	}
	usage, err := os.ReadFile(filepath.Join(outDir, "AI_USAGE.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(usage), report.AgentConfigSHA256) || strings.Contains(string(usage), "reasoning") {
		t.Fatalf("unexpected AI usage payload: %s", usage)
	}
}

func TestExecuteExportAllowsFailedRunReport(t *testing.T) {
	root, _, _, _ := exportFixture(t, "FAILED")
	var out, errout strings.Builder
	if code := invokeExport(root, filepath.Join(t.TempDir(), "failed-export"), &out, &errout); code != 0 {
		t.Fatalf("failed run should export: exit=%d stderr=%s", code, errout.String())
	}
}

func TestRequireRealExportDirectoryRejectsSymlinkParent(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows symlink creation requires platform-specific privileges")
	}
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias")
	if err := os.Symlink(actual, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := requireRealExportDirectory(filepath.Join(link, "child")); err == nil {
		t.Fatal("symlink parent was accepted")
	}
}

func TestExecuteExperimentExportDeduplicatesRunBundlesAndIndexesOmissions(t *testing.T) {
	base := t.TempDir()
	artifacts := filepath.Join(base, "artifacts")
	if err := os.Mkdir(artifacts, 0700); err != nil {
		t.Fatal(err)
	}
	runA, runB := "aaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbb"
	writeExperimentRun(t, artifacts, runA)
	writeExperimentRun(t, artifacts, runB)
	defaultOutputParent := filepath.Join(artifacts, "datasets")
	if err := os.Mkdir(defaultOutputParent, 0700); err != nil {
		t.Fatal(err)
	}
	input := experimentInput{SchemaVersion: 1, Name: "export-test", Tasks: []string{"task-a"}, Variants: []experimentVariant{{Agent: "scripted", Script: "success"}}, Repeats: 5}
	if err := validateExperiment(&input); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	inputHash := sha256.Sum256(canonical)
	experimentID := "0123456789abcdef0123456789abcdef"
	snapshot := jobs.TaskSnapshot{TaskRevision: 1, ManifestSHA256: strings.Repeat("a", 64), SourceSHA256: strings.Repeat("b", 64), EvaluatorSHA256: strings.Repeat("c", 64), ImageID: "sha256:" + strings.Repeat("d", 64), AgentConfigSHA256: strings.Repeat("e", 64)}
	started := time.Now().UTC()
	entries := make([]experimentEntry, 0, 5)
	for i, c := range experimentCases(input) {
		runID := ""
		status := jobs.StatusSucceeded
		switch i {
		case 0, 2:
			runID = runA
		case 1:
			runID = runB
		case 3:
			status = jobs.StatusQueued
		case 4:
			runID = "not-a-valid-run-id"
		}
		key := experimentIdempotencyKey(experimentID, c)
		job := &jobs.Job{ID: fmt.Sprintf("job-%d", i), Status: status, Request: requestForExperimentCase(c), TaskSnapshot: snapshot, IdempotencyKey: key, FinishedAt: &started, RunID: runID}
		if status == jobs.StatusQueued {
			job.FinishedAt = nil
		}
		stats := &experimentRunStats{RunID: runID}
		entries = append(entries, experimentEntry{Case: c, Request: requestForExperimentCase(c), TaskSnapshot: snapshot, IdempotencyKey: key, Job: job, Stats: stats})
	}
	manifest := experimentManifest{SchemaVersion: 1, ExperimentID: experimentID, Name: input.Name, InputSHA256: hex.EncodeToString(inputHash[:]), Input: input, StartedAt: started, UpdatedAt: started, Entries: entries}
	manifestPath := filepath.Join(base, "experiment.json")
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(defaultOutputParent, "export")
	var out, errout strings.Builder
	code := executeExperimentExport(context.Background(), []string{"--experiment", manifestPath, "--artifacts", artifacts, "--output", output}, &out, &errout)
	if code != 2 {
		t.Fatalf("partial experiment export should return 2, exit=%d stderr=%s stdout=%s", code, errout.String(), out.String())
	}
	for _, runID := range []string{runA, runB} {
		if _, err := os.Stat(filepath.Join(output, runID, "BUNDLE_MANIFEST.json")); err != nil {
			t.Fatalf("run bundle %s missing: %v", runID, err)
		}
	}
	entriesOnDisk, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesOnDisk) != 3 {
		t.Fatalf("expected two run bundles and index, got %d entries", len(entriesOnDisk))
	}
	indexBytes, err := os.ReadFile(filepath.Join(output, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index experimentExportIndex
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Records) != 5 {
		t.Fatalf("index records=%d", len(index.Records))
	}
	if index.Records[0].Status != "exported" || index.Records[0].Bundle != runA || index.Records[1].Status != "exported" || index.Records[1].Bundle != runB || index.Records[2].Status != "reused" || index.Records[2].Bundle != runA {
		t.Fatalf("unexpected exported/deduplicated records: %+v", index.Records[:3])
	}
	if index.Records[3].Status != "omitted" || !strings.Contains(index.Records[3].Reason, "not complete") {
		t.Fatalf("pending entry not recorded: %+v", index.Records[3])
	}
	if index.Records[4].Status != "omitted" || !strings.Contains(index.Records[4].Reason, "valid 24-hex") {
		t.Fatalf("invalid run ID not recorded: %+v", index.Records[4])
	}
	unsafeOutput := filepath.Join(artifacts, runA, "nested-export")
	var unsafeOut, unsafeErr strings.Builder
	if code := executeExperimentExport(context.Background(), []string{"--experiment", manifestPath, "--artifacts", artifacts, "--output", unsafeOutput}, &unsafeOut, &unsafeErr); code == 0 || !strings.Contains(unsafeErr.String(), "source run directory") {
		t.Fatalf("output inside a source run should be rejected: exit=%d stderr=%s", code, unsafeErr.String())
	}
}

func writeExperimentRun(t *testing.T, artifactsRoot, runID string) {
	t.Helper()
	runRoot := filepath.Join(artifactsRoot, runID)
	attemptID := runID + "-1"
	attemptDir := filepath.Join(runRoot, "attempts", attemptID)
	if err := os.MkdirAll(attemptDir, 0700); err != nil {
		t.Fatal(err)
	}
	patch := []byte("")
	trace := []byte("{\"sequence\":1}\n")
	if err := os.WriteFile(filepath.Join(attemptDir, "candidate.patch"), patch, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attemptDir, "trace.jsonl"), trace, 0600); err != nil {
		t.Fatal(err)
	}
	ph := sha256.Sum256(patch)
	report := runner.Report{SchemaVersion: 1, Version: runner.Version, RunID: runID, AttemptID: attemptID, TaskID: "task-a", TaskRevision: 1, Status: "SUCCEEDED", FinishedAt: time.Now().UTC(), PatchSHA256: hex.EncodeToString(ph[:]), Agent: "scripted", AgentConfigSHA256: strings.Repeat("e", 64), SourceSHA256: strings.Repeat("b", 64), EvaluatorSHA256: strings.Repeat("c", 64), ImageID: "sha256:" + strings.Repeat("d", 64), ArtifactDir: "C:\\untrusted\\elsewhere", Artifacts: map[string]string{"patch": filepath.ToSlash(filepath.Join("attempts", attemptID, "candidate.patch")), "trace": filepath.ToSlash(filepath.Join("attempts", attemptID, "trace.jsonl"))}}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runRoot, "result.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteExportRejectsUnsafeOrInconsistentInputsWithoutResidue(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, root string, report runner.Report)
	}{
		{name: "traversal", mutate: func(t *testing.T, root string, r runner.Report) {
			r.Artifacts["trace"] = "../outside.jsonl"
			rewriteExportReport(t, root, r)
		}},
		{name: "patch-hash", mutate: func(t *testing.T, root string, r runner.Report) {
			r.PatchSHA256 = strings.Repeat("0", 64)
			rewriteExportReport(t, root, r)
		}},
		{name: "run-id", mutate: func(t *testing.T, root string, r runner.Report) {
			r.RunID = "not-a-real-id"
			rewriteExportReport(t, root, r)
		}},
		{name: "nonterminal-status", mutate: func(t *testing.T, root string, r runner.Report) {
			r.Status = "RUNNING"
			rewriteExportReport(t, root, r)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, report, _, _ := exportFixture(t, "SUCCEEDED")
			tt.mutate(t, root, report)
			parent := t.TempDir()
			output := filepath.Join(parent, "bundle")
			var out, errout strings.Builder
			if code := invokeExport(root, output, &out, &errout); code == 0 {
				t.Fatal("unsafe export unexpectedly succeeded")
			}
			assertNoExportResidue(t, parent, output)
		})
	}
	t.Run("symlink", func(t *testing.T) {
		root, report, _, _ := exportFixture(t, "SUCCEEDED")
		attemptDir := filepath.Join(root, "attempts", report.AttemptID)
		real := filepath.Join(attemptDir, "real-trace.jsonl")
		if err := os.Rename(filepath.Join(attemptDir, "trace.jsonl"), real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(attemptDir, "trace.jsonl")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		parent := t.TempDir()
		output := filepath.Join(parent, "bundle")
		var out, errout strings.Builder
		if code := invokeExport(root, output, &out, &errout); code == 0 {
			t.Fatal("symlink artifact unexpectedly exported")
		}
		assertNoExportResidue(t, parent, output)
	})
	t.Run("existing-output", func(t *testing.T) {
		root, _, _, _ := exportFixture(t, "SUCCEEDED")
		parent := t.TempDir()
		output := filepath.Join(parent, "bundle")
		if err := os.Mkdir(output, 0700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(output, "keep.txt")
		if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		var out, errout strings.Builder
		if code := invokeExport(root, output, &out, &errout); code == 0 {
			t.Fatal("existing output unexpectedly overwritten")
		}
		got, err := os.ReadFile(marker)
		if err != nil || string(got) != "keep" {
			t.Fatalf("existing output changed: %q %v", got, err)
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("unexpected staging residue: %v", entries)
		}
	})
}

func rewriteExportReport(t *testing.T, root string, report runner.Report) {
	t.Helper()
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "result.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}
func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func assertNoExportResidue(t *testing.T, parent, output string) {
	t.Helper()
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("output remains after failure: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging residue remains: %v", entries)
	}
}
