package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalCommandsNeedNoDaemon(t *testing.T) {
	for _, args := range [][]string{{"version"}, {}, {"run", "--help"}} {
		var out, stderr bytes.Buffer
		if c := execute(context.Background(), args, &out, &stderr); c != 0 {
			t.Fatalf("%v exit %d: %s", args, c, stderr.String())
		}
	}
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "code.go"), []byte("package example\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if c := execute(context.Background(), []string{"hash", "--dir", d}, &out, &stderr); c != 0 || len(strings.TrimSpace(out.String())) != 64 {
		t.Fatal(c, out.String(), stderr.String())
	}
}
func TestGetCannotTraverseArtifactRoot(t *testing.T) {
	var out, stderr bytes.Buffer
	if c := execute(context.Background(), []string{"get", "../secret"}, &out, &stderr); c != 1 {
		t.Fatal(c)
	}
	if out.Len() != 0 {
		t.Fatal("unexpected data")
	}
}

func TestGetRejectsCorruptOrOversizedResultBeforeWriting(t *testing.T) {
	id := strings.Repeat("a", 24)
	root := t.TempDir()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{[]byte(`{"unfinished":`), bytes.Repeat([]byte(" "), (8<<20)+1)} {
		if err := os.WriteFile(filepath.Join(dir, "result.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		if c := execute(context.Background(), []string{"get", "--artifacts", root, id}, &out, &stderr); c != 1 || out.Len() != 0 {
			t.Fatal("get must not silently emit partial JSON", c, out.Len())
		}
	}
}
