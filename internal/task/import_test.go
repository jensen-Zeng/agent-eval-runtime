package task

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportSourceZipUnwrapsRootAndSkipsMetadata(t *testing.T) {
	base := t.TempDir()
	archivePath := filepath.Join(base, "repo.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	for name, body := range map[string]string{
		"repo/":            "",
		"repo/go.mod":      "module example.test/repo\n",
		"repo/main.go":     "package main\n",
		"repo/.git/config": "ignored\n",
		"repo/.cache/data": "ignored\n",
		"repo/._main.go":   "ignored\n",
		"repo/__MACOSX/x":  "ignored\n",
	} {
		h, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(base, "imported")
	if err := ImportSource(context.Background(), archivePath, dest); err != nil {
		t.Fatalf("ImportSource: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "go.mod")); err != nil || string(got) != "module example.test/repo\n" {
		t.Fatalf("go.mod = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "repo")); !os.IsNotExist(err) {
		t.Fatalf("repository prefix was not unwrapped: %v", err)
	}
	for _, name := range []string{".git", ".cache", "._main.go", "__MACOSX"} {
		if _, err := os.Lstat(filepath.Join(dest, name)); !os.IsNotExist(err) {
			t.Fatalf("metadata %s was imported: %v", name, err)
		}
	}
}

func TestImportSourceRejectsZipTraversalAndPreservesExistingDestination(t *testing.T) {
	base := t.TempDir()
	archivePath := filepath.Join(base, "bad.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(file)
	h, err := w.Create("../escape.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.Write([]byte("bad"))
	_ = w.Close()
	_ = file.Close()
	dest := filepath.Join(base, "dest")
	if err := os.Mkdir(dest, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dest, "marker")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ImportSource(context.Background(), archivePath, dest); err == nil {
		t.Fatal("traversal archive unexpectedly imported")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "preserve" {
		t.Fatalf("existing destination changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "escape.txt")); err == nil {
		t.Fatalf("archive escaped into parent: %q", got)
	}
}

func TestImportSourceTarGzip(t *testing.T) {
	base := t.TempDir()
	archivePath := filepath.Join(base, "repo.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for _, entry := range []struct {
		name string
		kind byte
		body string
	}{
		{name: "repo", kind: tar.TypeDir},
		{name: "repo/go.mod", kind: tar.TypeReg, body: "module example.test/tar\n"},
		{name: "repo/main.go", kind: tar.TypeReg, body: "package main\n"},
		{name: "repo/.git/config", kind: tar.TypeReg, body: "ignored\n"},
	} {
		hdr := &tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: 0600, Size: int64(len(entry.body))}
		if entry.kind == tar.TypeDir {
			hdr.Mode = 0700
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if entry.body != "" {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(base, "out")
	if err := ImportSource(context.Background(), archivePath, dest); err != nil {
		t.Fatalf("ImportSource: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "go.mod")); err != nil || !strings.Contains(string(got), "example.test/tar") {
		t.Fatalf("imported TAR go.mod = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, ".git")); !os.IsNotExist(err) {
		t.Fatalf("TAR metadata was imported: %v", err)
	}
}

func TestImportSourcePlainTar(t *testing.T) {
	base := t.TempDir()
	archivePath := filepath.Join(base, "repo.tar")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	body := "module example.test/plain-tar\n"
	if err := tw.WriteHeader(&tar.Header{Name: "repo/go.mod", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(base, "out")
	if err := ImportSource(context.Background(), archivePath, dest); err != nil {
		t.Fatalf("ImportSource: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "go.mod")); err != nil || string(got) != body {
		t.Fatalf("imported TAR go.mod = %q, %v", got, err)
	}
}

func TestImportSourceRejectsWindowsCollisionsAndLinks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*zip.Writer) error
	}{
		{
			name: "case-collision",
			write: func(w *zip.Writer) error {
				for _, name := range []string{"A.txt", "a.txt"} {
					h, err := w.Create(name)
					if err != nil {
						return err
					}
					if _, err := h.Write([]byte("x")); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			name: "symlink",
			write: func(w *zip.Writer) error {
				hdr := &zip.FileHeader{Name: "link", Method: zip.Store}
				hdr.SetMode(os.ModeSymlink | 0777)
				h, err := w.CreateHeader(hdr)
				if err != nil {
					return err
				}
				_, err = h.Write([]byte("target"))
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			archivePath := filepath.Join(base, "input.zip")
			f, err := os.Create(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			w := zip.NewWriter(f)
			if err := tc.write(w); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(base, "out")
			if err := ImportSource(context.Background(), archivePath, dest); err == nil {
				t.Fatal("unsafe archive unexpectedly imported")
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatalf("failed import left destination behind: %v", err)
			}
		})
	}
}
