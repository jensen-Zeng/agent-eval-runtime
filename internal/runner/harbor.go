package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"sort"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
)

// The archive is evidence, never an executable host-side extraction recipe.
// File names and executable bits are bounded again at this artifact boundary.
func submissionArchive(files sandbox.Files, modes map[string]uint32) ([]byte, error) {
	if len(files) > 4000 {
		return nil, fmt.Errorf("submission exceeds 4000 files")
	}
	names := make([]string, 0, len(files))
	total := 0
	for name, data := range files {
		if !task.ValidPath(name) || len(data) > 16<<20 {
			return nil, fmt.Errorf("invalid or oversized submission file")
		}
		total += len(data)
		if total > 64<<20 {
			return nil, fmt.Errorf("submission exceeds 64 MiB")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		data := files[name]
		mode := int64(0644 | (modes[name] & 0111))
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	if b.Len() > 72<<20 {
		return nil, fmt.Errorf("submission archive exceeds 72 MiB")
	}
	return b.Bytes(), nil
}
