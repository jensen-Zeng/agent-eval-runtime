package docker

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
)

func TestValidateSeedRejectsTraversalCollisionsAndOversize(t *testing.T) {
	one := []byte("x")
	fileData := make([]byte, task.MaxFileBytes+1)
	shared := make([]byte, task.MaxFileBytes)
	totalCount := task.MaxBundleBytes/task.MaxFileBytes + 1
	tooLargeTotal := make(sandbox.Files, totalCount)
	for i := 0; i < totalCount; i++ {
		tooLargeTotal[fmt.Sprintf("f%04d.go", i)] = shared
	}
	cases := []struct {
		name  string
		files sandbox.Files
	}{
		{"traversal", sandbox.Files{"../outside.go": one}},
		{"absolute", sandbox.Files{"/outside.go": one}},
		{"case-colliding-files", sandbox.Files{"A.go": one, "a.go": one}},
		{"case-file-directory-collision", sandbox.Files{"Pkg": one, "pkg/file.go": one}},
		{"file-size", sandbox.Files{"large.go": fileData}},
		{"bundle-size", tooLargeTotal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSeed(tc.files); err == nil {
				t.Fatal("unsafe or oversized seed accepted")
			}
		})
	}
}

func TestWriteSeedDeterministicAndOwnedBySandboxUser(t *testing.T) {
	filesA := sandbox.Files{"z.go": []byte("package z\n"), "pkg/a.go": []byte("package a\n"), "pkg/deep/b.go": []byte("package b\n")}
	filesB := sandbox.Files{"pkg/deep/b.go": []byte("package b\n"), "z.go": []byte("package z\n"), "pkg/a.go": []byte("package a\n")}
	var first, second bytes.Buffer
	if err := writeSeed(&first, filesA); err != nil {
		t.Fatal(err)
	}
	if err := writeSeed(&second, filesB); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("archive bytes depend on map iteration order")
	}
	tr := tar.NewReader(bytes.NewReader(first.Bytes()))
	entries := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries++
		if h.Uid != 10001 || h.Gid != 10001 {
			t.Fatalf("%s ownership=%d:%d", h.Name, h.Uid, h.Gid)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if h.Mode != 0755 {
				t.Fatalf("directory mode %o", h.Mode)
			}
		case tar.TypeReg:
			if h.Mode != 0644 {
				t.Fatalf("file mode %o", h.Mode)
			}
		default:
			t.Fatalf("unsafe archive entry type %d for %s", h.Typeflag, h.Name)
		}
	}
	if entries != 5 {
		t.Fatalf("got %d entries, expected two directories and three files", entries)
	}
}

func BenchmarkWriteSeed1000Files(b *testing.B) {
	files := make(sandbox.Files, 1000)
	for i := 0; i < 1000; i++ {
		files[fmt.Sprintf("pkg%04d/source.go", i)] = []byte("package sample\n")
	}
	b.ReportAllocs()
	b.SetBytes(int64(1000 * len("package sample\n")))
	for i := 0; i < b.N; i++ {
		if err := writeSeed(io.Discard, files); err != nil {
			b.Fatal(err)
		}
	}
}
