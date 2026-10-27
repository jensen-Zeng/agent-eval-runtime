package main

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExperimentMatrixExpansionAndKeys(t *testing.T) {
	in := experimentInput{SchemaVersion: 1, Name: "matrix", Tasks: []string{"task-a", "task-b"}, Variants: []experimentVariant{{Agent: "scripted", Script: "success"}, {Agent: "llm", Model: "model-x"}}, Repeats: 3}
	if err := validateExperiment(&in); err != nil {
		t.Fatal(err)
	}
	cases := experimentCases(in)
	if len(cases) != 12 {
		t.Fatalf("got %d cases", len(cases))
	}
	id := "0123456789abcdef0123456789abcdef"
	seen := map[string]bool{}
	for _, c := range cases {
		k := experimentIdempotencyKey(id, c)
		if len(k) > 128 || seen[k] {
			t.Fatalf("bad idempotency key %q", k)
		}
		seen[k] = true
	}
	if experimentIdempotencyKey(id, cases[0]) != experimentIdempotencyKey(id, cases[0]) {
		t.Fatal("case keys must be deterministic")
	}
}

func TestExperimentInputRejectsDuplicateAndOversizedMatrix(t *testing.T) {
	dup := experimentInput{SchemaVersion: 1, Name: "x", Tasks: []string{"t", "t"}, Variants: []experimentVariant{{Agent: "scripted", Script: "success"}}, Repeats: 1}
	if validateExperiment(&dup) == nil {
		t.Fatal("duplicate task accepted")
	}
	large := experimentInput{SchemaVersion: 1, Name: "x", Tasks: []string{"t"}, Variants: []experimentVariant{{Agent: "scripted", Script: "success"}}, Repeats: 101}
	if validateExperiment(&large) == nil {
		t.Fatal("repeat cap exceeded")
	}
	bad := experimentInput{SchemaVersion: 1, Name: "x", Tasks: []string{"t"}, Variants: []experimentVariant{{Agent: "scripted", Script: ""}}, Repeats: 1}
	if validateExperiment(&bad) == nil {
		t.Fatal("empty script accepted")
	}
}

func TestExperimentSummarySeparatesUnknownEstimatedAndInfrastructure(t *testing.T) {
	makeResult := func(quality string, wall int64) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"run_id": "abc", "wall_ms": wall, "usage": map[string]any{"total_tokens": 5, "quality": quality}, "resources": map[string]any{"agent": map[string]any{"cpu_seconds": 1.5, "memory_peak_sampled_bytes": uint64(100), "samples": 2}}})
		return b
	}
	entries := []experimentEntry{
		{Case: experimentCase{TaskID: "task-a", Agent: "llm", Model: "model", Repeat: 1}, Job: &jobs.Job{Status: jobs.StatusSucceeded, Result: makeResult("known", 10)}},
		{Case: experimentCase{TaskID: "task-a", Agent: "llm", Model: "model", Repeat: 2}, Job: &jobs.Job{Status: jobs.StatusInfraError}},
		{Case: experimentCase{TaskID: "task-a", Agent: "llm", Model: "model", Repeat: 3}, Job: &jobs.Job{Status: jobs.StatusRunning}},
		{Case: experimentCase{TaskID: "task-a", Agent: "llm", Model: "model", Repeat: 4}, Job: &jobs.Job{Status: jobs.StatusSucceeded, Result: makeResult("estimated", 20)}},
		{Case: experimentCase{TaskID: "task-a", Agent: "llm", Model: "model", Repeat: 5}},
	}
	s := summarizeExperiment(entries)
	if s.Total != 5 || s.Submitted != 4 || s.Succeeded != 2 || s.InfraErrors != 1 || s.Unknown != 2 {
		t.Fatalf("bad totals: %+v", s)
	}
	if len(s.Groups) != 1 {
		t.Fatal(len(s.Groups))
	}
	g := s.Groups[0]
	if g.TokenTotal != 5 || g.TokenEstimatedTotal != 5 || g.TokenEstimatedRuns != 1 || g.TokenUnknown != 3 {
		t.Fatalf("token buckets: %+v", g)
	}
	if g.WallMS != 30 || g.WallUnknown != 3 || g.CPUSeconds != 3 || g.CPUUnknown != 3 || g.MemoryPeakSampledBytes != 100 || g.MemoryUnknown != 3 {
		t.Fatalf("resource buckets: %+v", g)
	}
}

func TestExperimentResumeRejectsCorruptedFrozenEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	in := experimentInput{SchemaVersion: 1, Name: "x", Tasks: []string{"task-a"}, Variants: []experimentVariant{{Agent: "scripted", Script: "success"}}, Repeats: 1}
	encoded, _ := json.Marshal(in)
	sum := sha256.Sum256(encoded)
	hash := hex.EncodeToString(sum[:])
	id := "0123456789abcdef0123456789abcdef"
	c := experimentCases(in)[0]
	snapshot := jobs.TaskSnapshot{TaskRevision: 1, ManifestSHA256: strings.Repeat("a", 64), SourceSHA256: strings.Repeat("b", 64), EvaluatorSHA256: strings.Repeat("c", 64), ImageID: "sha256:" + strings.Repeat("e", 64), AgentConfigSHA256: strings.Repeat("d", 64)}
	m := experimentManifest{SchemaVersion: 1, ExperimentID: id, Name: in.Name, InputSHA256: hash, Input: in, Entries: []experimentEntry{{Case: c, Request: requestForExperimentCase(c), TaskSnapshot: snapshot, IdempotencyKey: experimentIdempotencyKey(id, c)}}}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateExperiment(path, in, hash, true); err != nil {
		t.Fatal(err)
	}
	m.Entries[0].TaskSnapshot.ImageID = "mutable-tag"
	b, _ = json.Marshal(m)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateExperiment(path, in, hash, true); err == nil {
		t.Fatal("corrupted snapshot accepted")
	}
}

func TestReevaluationPatchPathConfinement(t *testing.T) {
	allowed := []string{"parser.go"}
	valid := "diff --git a/parser.go b/parser.go\n--- a/parser.go\n+++ b/parser.go\n@@ -1 +1 @@\n-package old\n+package new\n"
	if err := validateReevaluationPatch(valid, allowed); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"diff --git a/../outside.go b/../outside.go\n--- a/../outside.go\n+++ b/../outside.go\n@@ -1 +1 @@\n-a\n+b\n",
		"diff --git a/parser_test.go b/parser_test.go\n--- a/parser_test.go\n+++ b/parser_test.go\n@@ -1 +1 @@\n-a\n+b\n",
		"diff --git a/parser.go b/parser.go\nnew file mode 120000\n--- /dev/null\n+++ b/parser.go\n",
	} {
		if err := validateReevaluationPatch(bad, allowed); err == nil {
			t.Fatal("unsafe patch accepted")
		}
	}
}
