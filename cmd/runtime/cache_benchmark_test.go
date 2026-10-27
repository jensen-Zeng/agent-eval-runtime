package main

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/config"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func BenchmarkCachedPrepare(b *testing.B) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("cannot locate benchmark source")
	}
	project := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	root := b.TempDir()
	for _, rel := range []string{"tasks/fixtures/go-parser-001", "evaluators/go-parser-001"} {
		src := filepath.Join(project, rel)
		if err := filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
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
				r, err := filepath.Rel(src, path)
				if err != nil {
					return err
				}
				target = filepath.Join(target, r)
			}
			if d.IsDir() {
				return os.MkdirAll(target, 0700)
			}
			if !info.Mode().IsRegular() {
				return os.ErrInvalid
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, 0600)
		}); err != nil {
			b.Fatalf("copy fixture %s: %v", rel, err)
		}
	}
	reg, err := newTaskRegistry(root, config.Default(), "sha256:"+strings.Repeat("f", 64))
	if err != nil {
		b.Fatal(err)
	}
	req := jobs.Request{SchemaVersion: 1, TaskID: "go-parser-001", Agent: "scripted", Script: "success"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := reg.prepare(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}
