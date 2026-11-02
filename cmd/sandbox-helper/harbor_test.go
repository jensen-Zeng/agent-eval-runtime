package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHarborSnapshotLimitsModesAndCacheFiltering(t *testing.T) {
	t.Setenv("AGENTEVAL_HARBOR", "1")
	t.Setenv("AGENTEVAL_WORKSPACE", "/app")
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := os.MkdirAll(filepath.Join(rootPath, "__pycache__"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, ".git", "objects"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, ".pytest_cache"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "node_modules", "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, ".venv"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "cache"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, ".cache"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "large.bin"), make([]byte, (1<<20)+17), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "run.sh"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"__pycache__/ignored.pyc", ".git/objects/ignored", ".pytest_cache/ignored", "node_modules/ignored", ".venv/ignored"} {
		if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(name)), []byte("cached"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"cache/result.json", ".cache/result.json"} {
		if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(name)), []byte(`{"required":true}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootPath, "node_modules", "pkg", "index.js"), []byte("module.exports=1"), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := handle(root, Request{Op: "snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Files["large.bin"]) != (1<<20)+17 {
		t.Fatalf("Harbor file limit rejected 1 MiB+ file, got %d bytes", len(out.Files["large.bin"]))
	}
	if runtime.GOOS != "windows" && out.Modes["run.sh"] != 0755 {
		t.Fatalf("executable mode=%o, want 0755", out.Modes["run.sh"])
	}
	for _, name := range []string{"__pycache__/ignored.pyc", ".git/objects/ignored", ".pytest_cache/ignored", "node_modules/ignored", ".venv/ignored"} {
		if _, ok := out.Files[name]; ok {
			t.Errorf("cache path %q included in Harbor snapshot", name)
		}
	}
	for _, name := range []string{"cache/result.json", ".cache/result.json"} {
		if _, ok := out.Files[name]; !ok {
			t.Fatalf("required/non-cache path %q omitted from Harbor snapshot", name)
		}
	}
	if got := string(out.Files["cache/result.json"]); got != `{"required":true}` {
		t.Fatalf("cache/result.json=%q", got)
	}
	if _, ok := out.Files["node_modules/pkg/index.js"]; ok {
		t.Fatal("node_modules metadata unexpectedly included in Harbor snapshot")
	}
}

func TestHelperWorkspaceSelectionAndModeLimits(t *testing.T) {
	t.Setenv("AGENTEVAL_WORKSPACE", "/app")
	t.Setenv("AGENTEVAL_HARBOR", "1")
	if got := workspacePath(); got != "/app" {
		t.Fatalf("workspacePath()=%q", got)
	}
	if maxFile, maxTotal, maxFiles := workspaceLimits(); maxFile != 16<<20 || maxTotal != 64<<20 || maxFiles != 4000 {
		t.Fatalf("Harbor limits=(%d,%d,%d)", maxFile, maxTotal, maxFiles)
	}
	t.Setenv("AGENTEVAL_HARBOR", "")
	if got := workspacePath(); got != "/app" {
		t.Fatalf("workspacePath()=%q", got)
	}
	if maxFile, maxTotal, maxFiles := workspaceLimits(); maxFile != 1<<20 || maxTotal != 50<<20 || maxFiles != 1000 {
		t.Fatalf("Go limits changed=(%d,%d,%d)", maxFile, maxTotal, maxFiles)
	}
}
