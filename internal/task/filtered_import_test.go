package task

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitSourceExclusionKeepsLimitsAndRecordsRealSource(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "source.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	entries := map[string]string{"snapshot/go.mod": "module test.example\n", "snapshot/main.go": "package main\n", "snapshot/reports/coverage.html": strings.Repeat("x", MaxFileBytes+1)}
	for n, b := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0600, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(b))
	}
	tw.Close()
	gz.Close()
	f.Close()
	if err := ImportSource(context.Background(), archive, filepath.Join(dir, "unfiltered")); err == nil {
		t.Fatal("normal size limit bypassed")
	}
	if err := ImportSourceFiltered(context.Background(), archive, filepath.Join(dir, "filtered"), []string{"snapshot/reports"}); err != nil {
		t.Fatal(err)
	}
	files, err := ReadBundle(filepath.Join(dir, "filtered"))
	if err != nil || len(files) != 2 {
		t.Fatal(files, err)
	}
	if err := ImportSourceFiltered(context.Background(), archive, filepath.Join(dir, "bad"), []string{"../reports"}); err == nil {
		t.Fatal("unsafe exclusion accepted")
	}
}
