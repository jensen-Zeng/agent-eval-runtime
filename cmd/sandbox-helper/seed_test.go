package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type seedArchiveEntry struct {
	header tar.Header
	body   []byte
}

func seedArchive(t *testing.T, entries ...seedArchiveEntry) []byte {
	t.Helper()
	var data bytes.Buffer
	w := tar.NewWriter(&data)
	for _, entry := range entries {
		h := entry.header
		h.Size = int64(len(entry.body))
		if err := w.WriteHeader(&h); err != nil {
			t.Fatalf("write tar header %q: %v", h.Name, err)
		}
		if len(entry.body) != 0 {
			if _, err := w.Write(entry.body); err != nil {
				t.Fatalf("write tar body %q: %v", h.Name, err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func safeSeedFile(name string, body []byte) seedArchiveEntry {
	return seedArchiveEntry{header: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Uid: 10001, Gid: 10001}, body: body}
}

func runSeedArchive(t *testing.T, data []byte) (string, error) {
	t.Helper()
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = seedFromTar(root, bytes.NewReader(data))
	return rootPath, err
}

func TestSeedFromTarRejectsTraversalAndAbsolutePaths(t *testing.T) {
	for _, name := range []string{"../outside.txt", filepath.ToSlash(filepath.Join(string(filepath.Separator), "outside.txt"))} {
		t.Run(name, func(t *testing.T) {
			root, err := runSeedArchive(t, seedArchive(t, safeSeedFile(name, []byte("bad"))))
			if err == nil {
				t.Fatalf("accepted escaping path %q", name)
			}
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(root), "outside.txt")); statErr == nil {
				t.Fatal("archive wrote outside the destination root")
			}
		})
	}
}

func TestSeedFromTarRejectsLinksDuplicatesAndCaseCollisions(t *testing.T) {
	tests := []struct {
		name    string
		entries []seedArchiveEntry
	}{
		{name: "symlink", entries: []seedArchiveEntry{{header: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../outside", Mode: 0777, Uid: 10001, Gid: 10001}}}},
		{name: "hardlink", entries: []seedArchiveEntry{safeSeedFile("target", []byte("x")), {header: tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "target", Mode: 0644, Uid: 10001, Gid: 10001}}}},
		{name: "duplicate-path", entries: []seedArchiveEntry{safeSeedFile("same.txt", []byte("one")), safeSeedFile("same.txt", []byte("two"))}},
		{name: "case-collision", entries: []seedArchiveEntry{safeSeedFile("File.txt", []byte("one")), safeSeedFile("file.txt", []byte("two"))}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := runSeedArchive(t, seedArchive(t, test.entries...)); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestSeedFromTarRejectsFileAndCountLimits(t *testing.T) {
	t.Run("file-size", func(t *testing.T) {
		body := make([]byte, maxFile+1)
		_, err := runSeedArchive(t, seedArchive(t, safeSeedFile("large.bin", body)))
		if err == nil {
			t.Fatal("oversized file accepted")
		}
	})
	t.Run("file-count", func(t *testing.T) {
		entries := make([]seedArchiveEntry, maxFiles+1)
		for i := range entries {
			entries[i] = safeSeedFile(fmt.Sprintf("f%06d", i), []byte("x"))
		}
		_, err := runSeedArchive(t, seedArchive(t, entries...))
		if err == nil {
			t.Fatal("archive above file count limit accepted")
		}
	})
}

func TestSeedFromTarRejectsUnsafeOwnershipAndModes(t *testing.T) {
	uid := safeSeedFile("root-owned", []byte("x"))
	uid.header.Uid = 0
	mode := safeSeedFile("executable", []byte("x"))
	mode.header.Mode = 0777
	for _, tc := range []struct {
		name  string
		entry seedArchiveEntry
	}{{"uid", uid}, {"mode", mode}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := runSeedArchive(t, seedArchive(t, tc.entry)); err == nil {
				t.Fatal("archive metadata outside seed policy accepted")
			}
		})
	}
}

func TestSeedFromTarWritesOnlyRegularOwnedFiles(t *testing.T) {
	archive := seedArchive(t,
		seedArchiveEntry{header: tar.Header{Name: "nested/", Typeflag: tar.TypeDir, Mode: 0755, Uid: 10001, Gid: 10001}},
		safeSeedFile("nested/file.txt", []byte("payload")),
	)
	root, err := runSeedArchive(t, archive)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "nested", "file.txt"))
	if err != nil || string(got) != "payload" {
		t.Fatalf("seeded file=%q err=%v", got, err)
	}
	info, err := os.Stat(filepath.Join(root, "nested", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0644 {
		t.Fatalf("seeded mode=%o, want 0644", info.Mode().Perm())
	}
}

func TestSeedFromTarUsesHarborFileLimitsAndExecutableMode(t *testing.T) {
	t.Setenv("AGENTEVAL_HARBOR", "1")
	body := make([]byte, (1<<20)+31)
	entry := safeSeedFile("run.sh", body)
	entry.header.Mode = 0755
	root, err := runSeedArchive(t, seedArchive(t, entry))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0755 {
		t.Fatalf("seeded mode=%o, want 0755", info.Mode().Perm())
	}
}

func TestSeedFromTarOverwritesRegularImageSeedFiles(t *testing.T) {
	t.Setenv("AGENTEVAL_HARBOR", "1")
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "initial.json"), []byte("image"), 0644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := seedFromTar(root, bytes.NewReader(seedArchive(t, safeSeedFile("initial.json", []byte("submission"))))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(rootPath, "initial.json"))
	if err != nil || string(got) != "submission" {
		t.Fatalf("overlaid image seed=%q err=%v", got, err)
	}
}
