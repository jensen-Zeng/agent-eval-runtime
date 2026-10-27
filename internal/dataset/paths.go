package dataset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
)

// Resolve safely resolves a catalog-relative path. It rejects links in every
// path component and opens the target relative to an os.Root before returning
// the host path needed by existing import and grader interfaces.
func Resolve(root, p string) (string, error) {
	absRoot, err := realRoot(root)
	if err != nil {
		return "", err
	}
	if !validRelative(p, true) {
		return "", fmt.Errorf("path must be a safe relative path")
	}
	if p == "." {
		return absRoot, nil
	}
	parts := strings.Split(p, "/")
	current := absRoot
	for i, part := range parts {
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink path components are forbidden")
		}
		if i != len(parts)-1 && !st.IsDir() {
			return "", fmt.Errorf("path component is not a directory")
		}
	}
	secure, err := os.OpenRoot(absRoot)
	if err != nil {
		return "", err
	}
	defer secure.Close()
	f, err := secure.Open(filepath.FromSlash(p))
	if err != nil {
		return "", err
	}
	if _, err := f.Stat(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return current, nil
}

// realRoot requires an absolute, existing directory whose components are not
// symlinks. It returns a cleaned absolute path without resolving links.
func realRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("root must be an absolute path")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	volume := filepath.VolumeName(abs)
	current := volume + string(filepath.Separator)
	for _, component := range strings.FieldsFunc(strings.TrimPrefix(abs, volume), func(r rune) bool { return r == '/' || r == '\\' }) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		st, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("root contains a symlink component")
		}
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return "", fmt.Errorf("root must be a real directory")
	}
	return abs, nil
}

// VerifyInputs rejects stale catalog entries before a run. It hashes the raw
// source archive/tree, selected tar member, prompt files, and every file under
// the trusted grader directory. Grader contents are measured, never returned.
func VerifyInputs(ctx context.Context, root string, item Item) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if err := validateCatalogItem(item); err != nil {
		return err
	}
	if _, err := realRoot(root); err != nil {
		return err
	}
	if item.Source != "" {
		p, err := Resolve(root, item.Source)
		if err != nil {
			return fmt.Errorf("resolve source: %w", err)
		}
		want := item.ExpectedSHA256[item.Source]
		if want != "" {
			got, err := hashPath(ctx, p)
			if err != nil {
				return fmt.Errorf("hash source: %w", err)
			}
			if !strings.EqualFold(got, want) {
				return fmt.Errorf("source SHA-256 changed since scan")
			}
		}
		if item.SourceMember != "" {
			want := item.ExpectedSHA256[item.Source+"!"+item.SourceMember]
			if want != "" {
				idx, err := inspectTar(ctx, p, nil)
				if err != nil {
					return err
				}
				got := snapshotDigest(idx, item.SourceMember)
				if !strings.EqualFold(got, want) {
					return fmt.Errorf("source member SHA-256 changed since scan")
				}
			}
		}
	}
	for _, rel := range append(append([]string(nil), item.PromptFiles...), graderFiles(item)...) {
		want := item.ExpectedSHA256[rel]
		if want == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p, err := Resolve(root, rel)
		if err != nil {
			return fmt.Errorf("resolve protected input %s: %w", rel, err)
		}
		got, err := hashFileBounded(ctx, p, task.MaxFileBytes)
		if err != nil {
			return fmt.Errorf("hash protected input %s: %w", rel, err)
		}
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("protected input changed since scan: %s", rel)
		}
	}
	return nil
}

func validateCatalogItem(item Item) error {
	if !keyPattern.MatchString(item.Key) || !taskPattern.MatchString(item.TaskID) || !validState(item.State) {
		return fmt.Errorf("invalid catalog item")
	}
	if item.Source != "" && !validRelative(item.Source, false) {
		return fmt.Errorf("invalid source path")
	}
	if item.SourceMember != "" && !validMember(item.SourceMember) {
		return fmt.Errorf("invalid source member")
	}
	for _, p := range item.PromptFiles {
		if !validRelative(p, false) {
			return fmt.Errorf("invalid prompt path")
		}
	}
	for _, p := range item.PromptMembers {
		if !validPromptMember(p) {
			return fmt.Errorf("invalid prompt member")
		}
	}
	for _, p := range item.SourceExcludes {
		if !validMember(p) {
			return fmt.Errorf("invalid source exclusion")
		}
	}
	for p, h := range item.ExpectedSHA256 {
		if !shaPattern.MatchString(h) || (!validRelative(p, false) && !strings.Contains(p, "!")) {
			return fmt.Errorf("invalid expected hash")
		}
	}
	return nil
}

func graderFiles(item Item) []string {
	var out []string
	if item.GraderDir == "" {
		return out
	}
	for p := range item.ExpectedSHA256 {
		if strings.HasPrefix(p, item.GraderDir+"/") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func hashPath(ctx context.Context, name string) (string, error) {
	st, err := os.Lstat(name)
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("symlink is forbidden")
	}
	if st.IsDir() {
		return directoryDigest(ctx, name)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file or directory")
	}
	return hashFileBounded(ctx, name, maxOuterCompressed)
}

func hashFileBounded(ctx context.Context, name string, max int64) (string, error) {
	st, err := os.Lstat(name)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() || st.Size() < 0 || st.Size() > max {
		return "", fmt.Errorf("file is linked or exceeds hash limit")
	}
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, readErr := copyContext(ctx, h, io.LimitReader(f, max+1))
	after, statErr := f.Stat()
	closeErr := f.Close()
	if readErr != nil {
		return "", readErr
	}
	if statErr != nil {
		return "", statErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if after.Size() != st.Size() {
		return "", fmt.Errorf("file changed while hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func directoryDigest(ctx context.Context, dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	files := make(map[string][]byte)
	total := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name = filepath.ToSlash(name)
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in source directory")
		}
		if ignoredImportPath(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() || st.Size() < 0 || st.Size() > task.MaxFileBytes {
			return fmt.Errorf("source file violates import limits")
		}
		if len(files) >= 1000 {
			return fmt.Errorf("source file count exceeds import limit")
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		b, readErr := io.ReadAll(io.LimitReader(f, task.MaxFileBytes+1))
		closeErr := f.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if int64(len(b)) != st.Size() {
			return fmt.Errorf("source file changed while hashing")
		}
		total += len(b)
		if total > task.MaxBundleBytes {
			return fmt.Errorf("source directory exceeds import size limit")
		}
		files[name] = b
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("source directory is empty")
	}
	return task.HashFiles(files), nil
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			m, werr := dst.Write(buf[:n])
			total += int64(m)
			if werr != nil {
				return total, werr
			}
			if m != n {
				return total, io.ErrShortWrite
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}
