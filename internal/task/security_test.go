package task

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadBundleRejectsGitMetadataAtEveryPathLevel(t *testing.T) {
	for _, name := range []string{".git/config", "nested/.GiT/hooks/pre-commit", "pkg/.Git/index"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("metadata"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadBundle(root); err == nil {
				t.Fatalf("Git metadata path %q was accepted", name)
			}
		})
	}
}

func TestReadBundleAllowsGitHubSourcePaths(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, ".github", "workflows", "ci.yml")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("name: CI\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBundle(root); err != nil {
		t.Fatalf("valid .github source path rejected: %v", err)
	}
}
