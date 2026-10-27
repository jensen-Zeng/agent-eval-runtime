package dataset

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
)

// MaterializeSource imports only the declared source tree into a new directory.
// When SourceMember selects a subtree inside an outer TAR, every outer member is
// validated, but only the selected subtree is staged and passed to task import.
func MaterializeSource(ctx context.Context, root string, item Item, dest string) error {
	if err := VerifyInputs(ctx, root, item); err != nil {
		return err
	}
	source, err := Resolve(root, item.Source)
	if err != nil {
		return fmt.Errorf("resolve source: %w", err)
	}
	if item.SourceMember == "" {
		return task.ImportSourceFiltered(ctx, source, dest, append([]string(nil), item.SourceExcludes...))
	}
	idx, err := inspectTar(ctx, source, nil)
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "agenteval-dataset-source-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	if err := os.Chmod(temp, 0700); err != nil {
		return err
	}
	// A member may name one nested archive. ImportSourceFiltered will then
	// validate that archive using the regular source-import policy.
	for _, f := range idx.files {
		if f.name != item.SourceMember {
			continue
		}
		if !isSupportedArchive(f.name) {
			return fmt.Errorf("selected TAR member is not a supported source archive")
		}
		if len(f.data) > maxOuterCompressed {
			return fmt.Errorf("selected source archive exceeds compressed size limit")
		}
		archive := filepath.Join(temp, filepath.Base(filepath.FromSlash(f.name)))
		if err := os.WriteFile(archive, f.data, 0600); err != nil {
			return err
		}
		return task.ImportSourceFiltered(ctx, archive, dest, append([]string(nil), item.SourceExcludes...))
	}
	prefix := item.SourceMember + "/"
	selected := 0
	for _, f := range idx.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasPrefix(f.name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(f.name, prefix)
		if rel == "" || !validMember(rel) || ignoredImportPath(rel) {
			continue
		}
		full := filepath.Join(temp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			return err
		}
		out, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := io.Copy(out, bytes.NewReader(f.data))
		closeErr := out.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		selected++
	}
	if selected == 0 {
		return fmt.Errorf("selected source member contains no files")
	}
	excludes := make([]string, 0, len(item.SourceExcludes))
	for _, excluded := range item.SourceExcludes {
		if strings.HasPrefix(excluded, prefix) {
			excluded = strings.TrimPrefix(excluded, prefix)
		}
		if excluded != "" {
			excludes = append(excludes, excluded)
		}
	}
	return task.ImportSourceFiltered(ctx, temp, dest, excludes)
}

// ReadPrompts concatenates only prompt paths explicitly stored in the catalog.
// It verifies the catalog's source, prompt, and grader hashes before reading.
func ReadPrompts(ctx context.Context, root string, item Item) (string, error) {
	if err := VerifyInputs(ctx, root, item); err != nil {
		return "", err
	}
	var out strings.Builder
	total := 0
	appendPrompt := func(name string, data []byte) error {
		if !utf8.Valid(data) {
			return fmt.Errorf("prompt %s is not strict UTF-8", name)
		}
		data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
		separator := 0
		if out.Len() > 0 {
			separator = 2
		}
		header := "--- " + name + " ---\n"
		needed := separator + len(header) + len(data)
		if total+needed > MaxPromptBytes {
			return fmt.Errorf("combined prompt exceeds %d bytes", MaxPromptBytes)
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(header)
		out.Write(data)
		total += needed
		return nil
	}
	for _, rel := range item.PromptFiles {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		name, err := Resolve(root, rel)
		if err != nil {
			return "", err
		}
		data, err := readBoundedFile(name, MaxPromptBytes)
		if err != nil {
			return "", err
		}
		if err := appendPrompt(rel, data); err != nil {
			return "", err
		}
	}
	if len(item.PromptMembers) > 0 {
		source, err := Resolve(root, item.Source)
		if err != nil {
			return "", err
		}
		members, err := readArchivePrompts(ctx, source, item, item.PromptMembers)
		if err != nil {
			return "", err
		}
		for _, p := range item.PromptMembers {
			data, ok := members[p]
			if !ok {
				return "", fmt.Errorf("prompt member %s is missing", p)
			}
			if err := appendPrompt(item.Source+"!"+p, data); err != nil {
				return "", err
			}
		}
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("catalog item has no prompt files")
	}
	return out.String(), nil
}

func readArchivePrompts(ctx context.Context, source string, item Item, members []string) (map[string][]byte, error) {
	result := make(map[string][]byte, len(members))
	lower := strings.ToLower(source)
	if strings.HasSuffix(lower, ".zip") {
		if _, _, err := inspectZipMeta(ctx, source); err != nil {
			return nil, err
		}
		return readZipPrompts(ctx, source, members)
	}
	idx, err := inspectTar(ctx, source, nil)
	if err != nil {
		return nil, err
	}
	outer := make(map[string][]byte, len(idx.files))
	for _, f := range idx.files {
		outer[f.name] = f.data
	}
	for _, member := range members {
		if strings.Contains(member, "!") {
			parts := strings.SplitN(member, "!", 2)
			if item.SourceMember == "" || parts[0] != item.SourceMember || !validMember(parts[1]) {
				return nil, fmt.Errorf("invalid nested prompt member")
			}
			archiveData, ok := outer[parts[0]]
			if !ok {
				return nil, fmt.Errorf("nested source archive is missing")
			}
			tmp, err := os.MkdirTemp("", "agenteval-dataset-prompt-")
			if err != nil {
				return nil, err
			}
			name := filepath.Join(tmp, filepath.Base(filepath.FromSlash(parts[0])))
			writeErr := os.WriteFile(name, archiveData, 0600)
			if writeErr == nil {
				if strings.HasSuffix(strings.ToLower(name), ".zip") {
					var nested map[string][]byte
					nested, err = readZipPrompts(ctx, name, []string{parts[1]})
					if err == nil {
						result[member] = nested[parts[1]]
					}
				} else {
					inner, innerErr := inspectTar(ctx, name, nil)
					if innerErr != nil {
						err = innerErr
					} else {
						for _, f := range inner.files {
							if f.name == parts[1] {
								result[member] = f.data
								break
							}
						}
					}
				}
			}
			_ = os.RemoveAll(tmp)
			if writeErr != nil {
				return nil, writeErr
			}
			if err != nil {
				return nil, err
			}
			if _, ok := result[member]; !ok {
				return nil, fmt.Errorf("nested prompt member is missing")
			}
			continue
		}
		if data, ok := outer[member]; ok {
			result[member] = data
		}
	}
	return result, nil
}

func readZipPrompts(ctx context.Context, archive string, names []string) (map[string][]byte, error) {
	var result = make(map[string][]byte, len(names))
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name, dir, err := cleanArchiveName(f.Name)
		if err != nil {
			return nil, err
		}
		if dir {
			continue
		}
		if _, ok := wanted[name]; !ok {
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 || !f.Mode().IsRegular() || f.UncompressedSize64 > MaxPromptBytes {
			return nil, fmt.Errorf("unsafe or oversized prompt member")
		}
		r, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(r, MaxPromptBytes+1))
		closeErr := r.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(data) > MaxPromptBytes {
			return nil, fmt.Errorf("prompt member exceeds size limit")
		}
		result[name] = data
	}
	return result, nil
}

func isSupportedArchive(name string) bool {
	name = strings.ToLower(name)
	return strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz")
}
