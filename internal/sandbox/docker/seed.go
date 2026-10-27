package docker

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// Seed creates one bounded archive from immutable byte bundles, avoiding an exec
// round trip per file. This is used only before untrusted execution starts.
func (s *instance) seed(ctx context.Context, files sandbox.Files) error {
	if err := validateSeed(files); err != nil {
		return err
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { err := writeSeed(writer, files); _ = writer.CloseWithError(err); done <- err }()
	res, err := s.execStreamInput(ctx, []string{"/usr/local/bin/sandbox-helper", "--seed"}, reader)
	_ = reader.CloseWithError(err)
	writeErr := <-done
	if err != nil {
		return fmt.Errorf("copy source bundle: %w", err)
	}
	if writeErr != nil {
		return writeErr
	}
	var answer helperResponse
	if !res.ExitKnown || res.ExitCode != 0 || json.Unmarshal([]byte(res.Stdout), &answer) != nil || !answer.OK {
		return fmt.Errorf("seed helper rejected source: %s", answer.Error)
	}
	return nil
}
func validateSeed(files sandbox.Files) error {
	if len(files) > 1000 {
		return fmt.Errorf("seed exceeds file limit")
	}
	total := 0
	names := map[string]bool{}
	for n, b := range files {
		if !task.ValidPath(n) || len(b) > task.MaxFileBytes {
			return fmt.Errorf("invalid seed path or file size")
		}
		total += len(b)
		if total > task.MaxBundleBytes {
			return fmt.Errorf("seed exceeds byte limit")
		}
		folded := strings.ToLower(n)
		if names[folded] {
			return fmt.Errorf("case-colliding seed paths")
		}
		names[folded] = true
	}
	for n := range files {
		for p := path.Dir(n); p != "."; p = path.Dir(p) {
			if names[strings.ToLower(p)] {
				return fmt.Errorf("file/directory seed collision")
			}
		}
	}
	return nil
}
func writeSeed(w io.Writer, files sandbox.Files) error {
	tw := tar.NewWriter(w)
	names := make([]string, 0, len(files))
	dirs := map[string]bool{}
	for n := range files {
		names = append(names, n)
		for p := path.Dir(n); p != "."; p = path.Dir(p) {
			dirs[p] = true
		}
	}
	dn := make([]string, 0, len(dirs))
	for d := range dirs {
		dn = append(dn, d)
	}
	sort.Strings(dn)
	sort.Strings(names)
	for _, n := range dn {
		if err := tw.WriteHeader(&tar.Header{Name: n + "/", Typeflag: tar.TypeDir, Mode: 0755, Uid: 10001, Gid: 10001}); err != nil {
			return err
		}
	}
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(files[n])), Uid: 10001, Gid: 10001}); err != nil {
			return err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return err
		}
	}
	return tw.Close()
}
