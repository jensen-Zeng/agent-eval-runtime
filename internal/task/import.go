package task

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxImportFiles     = 1000
	maxImportDirs      = 2000
	maxArchiveEntries  = maxImportFiles + maxImportDirs
	maxArchiveBytes    = 64 << 20
	maxTarExpanded     = MaxBundleBytes + (16 << 20)
	maxImportPathUnits = 240
)

type importEntry struct {
	name string
	dir  bool
	data []byte
}

type importNode struct {
	dir      bool
	explicit bool
}

// ImportSource copies a directory or ZIP/TAR source tree into dest. The destination
// must not exist. Files are validated and staged in a private sibling directory;
// the destination appears only after the complete import succeeds. If the input
// contains exactly one top-level directory with a root go.mod, that directory is
// automatically unwrapped. Metadata directories (.git, .cache, __MACOSX) and
// AppleDouble files (._*) are omitted.
func ImportSource(ctx context.Context, source, dest string) error {
	return ImportSourceFiltered(ctx, source, dest, nil)
}

// ImportSourceFiltered omits only operator-selected original input paths.
// Archive exclusions include the original archive root prefix. Exclusions are
// recorded by the dataset layer; all remaining files retain the normal limits.
// No exclusion permits links, special files, unsafe paths or oversized output.
func ImportSourceFiltered(ctx context.Context, source, dest string, excluded []string) error {
	if len(excluded) > 32 {
		return fmt.Errorf("too many explicit exclusions")
	}
	for _, p := range excluded {
		if !validImportPath(p) || strings.HasSuffix(p, "/") {
			return fmt.Errorf("invalid explicit exclusion %q", p)
		}
	}
	if ctx == nil || strings.TrimSpace(source) == "" || strings.TrimSpace(dest) == "" {
		return fmt.Errorf("source, destination, and context are required")
	}
	absDest, parent, err := validateDestination(dest)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(absDest); err == nil {
		return fmt.Errorf("destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect destination: %w", err)
	}
	absSource, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve source: %w", err)
	}
	st, err := os.Lstat(absSource)
	if err != nil {
		return fmt.Errorf("inspect source: %w", err)
	}
	var entries []importEntry
	var verify func() error
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source symlinks are forbidden")
	}
	if st.IsDir() {
		entries, verify, err = readDirectorySource(ctx, absSource, excluded...)
	} else if st.Mode().IsRegular() {
		switch {
		case strings.EqualFold(filepath.Ext(absSource), ".zip"):
			entries, verify, err = readZipSource(ctx, absSource, excluded...)
		case strings.EqualFold(filepath.Ext(absSource), ".tar") || strings.EqualFold(filepath.Ext(absSource), ".tgz") || strings.HasSuffix(strings.ToLower(absSource), ".tar.gz"):
			entries, verify, err = readTarSource(ctx, absSource, excluded...)
		default:
			return fmt.Errorf("source must be a directory, ZIP, TAR, or TAR.GZ archive")
		}
	} else {
		return fmt.Errorf("source must be a directory, ZIP, TAR, or TAR.GZ archive")
	}
	if err != nil {
		return err
	}
	sortImportEntries(entries)
	entries, err = normalizeImportRoot(entries)
	if err != nil {
		return err
	}
	if err := validateImportTree(entries); err != nil {
		return err
	}

	stage, err := os.MkdirTemp(parent, ".agenteval-import-")
	if err != nil {
		return fmt.Errorf("create import staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0700); err != nil {
		return fmt.Errorf("secure import staging directory: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		full := filepath.Join(stage, filepath.FromSlash(entry.name))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			return fmt.Errorf("create imported directory: %w", err)
		}
		if entry.dir {
			if err := os.MkdirAll(full, 0700); err != nil {
				return fmt.Errorf("create imported directory: %w", err)
			}
			continue
		}
		f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("create imported file %q: %w", entry.name, err)
		}
		_, writeErr := f.Write(entry.data)
		closeErr := f.Close()
		if writeErr != nil {
			return fmt.Errorf("write imported file %q: %w", entry.name, writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close imported file %q: %w", entry.name, closeErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if verify != nil {
		if err := verify(); err != nil {
			return fmt.Errorf("source changed during import: %w", err)
		}
	}
	if _, err := os.Lstat(absDest); err == nil {
		return fmt.Errorf("destination appeared during import")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("recheck destination: %w", err)
	}
	if err := os.Rename(stage, absDest); err != nil {
		return fmt.Errorf("publish imported source: %w", err)
	}
	committed = true
	return nil
}

func validateDestination(dest string) (string, string, error) {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", "", fmt.Errorf("resolve destination: %w", err)
	}
	if filepath.Clean(abs) == filepath.VolumeName(abs)+string(filepath.Separator) || filepath.Base(abs) == "." || filepath.Base(abs) == string(filepath.Separator) {
		return "", "", fmt.Errorf("destination must name a new directory")
	}
	parent := filepath.Dir(abs)
	if err := requireRealDirectoryPath(parent); err != nil {
		return "", "", err
	}
	return abs, parent, nil
}

func requireRealDirectoryPath(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	rest := strings.TrimPrefix(abs, volume)
	current := volume + string(filepath.Separator)
	for _, component := range strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' }) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect destination parent: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("destination parent must not contain symlinks and must be a directory")
		}
	}
	return nil
}

func readDirectorySource(ctx context.Context, source string, excluded ...string) ([]importEntry, func() error, error) {
	rootInfo, err := os.Lstat(source)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, nil, fmt.Errorf("source directory changed while opening")
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return nil, nil, fmt.Errorf("open source directory: %w", err)
	}
	defer root.Close()
	entries := make([]importEntry, 0)
	hashes := make(map[string][32]byte)
	fileCount, dirCount, total := 0, 0, 0
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
		name = path.Clean(name)
		if !utf8.ValidString(name) || !validImportPath(name) {
			return fmt.Errorf("invalid source path %q", name)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are forbidden: %s", name)
		}
		if skipExplicitImportPath(name, excluded) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			dirCount++
			if dirCount > maxImportDirs {
				return fmt.Errorf("source directory limit exceeded")
			}
			entries = append(entries, importEntry{name: name, dir: true})
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special files are forbidden: %s", name)
		}
		fileCount++
		if fileCount > maxImportFiles || info.Size() < 0 || info.Size() > MaxFileBytes {
			return fmt.Errorf("source file limit exceeded")
		}
		data, before, after, err := readRootFile(ctx, root, name)
		if err != nil {
			return err
		}
		if !sameFileSnapshot(info, before) || !sameFileSnapshot(before, after) {
			return fmt.Errorf("source file changed while reading: %s", name)
		}
		total += len(data)
		if total > MaxBundleBytes {
			return fmt.Errorf("source bundle size exceeded")
		}
		hashes[name] = sha256.Sum256(data)
		entries = append(entries, importEntry{name: name, data: data})
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("read source directory: %w", err)
	}
	if fileCount == 0 {
		return nil, nil, fmt.Errorf("source contains no files")
	}
	verify := func() error {
		current, err := os.Lstat(source)
		if err != nil || current.Mode()&os.ModeSymlink != 0 || !sameFileSnapshot(rootInfo, current) {
			return fmt.Errorf("source directory changed during import")
		}
		return verifyDirectorySnapshot(ctx, source, hashes, excluded...)
	}
	return entries, verify, nil
}

func readRootFile(ctx context.Context, root *os.Root, name string) ([]byte, fs.FileInfo, fs.FileInfo, error) {
	before, err := root.Stat(name)
	if err != nil {
		return nil, nil, nil, err
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > MaxFileBytes {
		return nil, nil, nil, fmt.Errorf("source file is not a bounded regular file: %s", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, nil, err
	}
	opened, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return nil, nil, nil, statErr
	}
	if !sameFileSnapshot(before, opened) {
		_ = f.Close()
		return nil, nil, nil, fmt.Errorf("source file changed while opening: %s", name)
	}
	data, readErr := readBounded(ctx, f, MaxFileBytes)
	after, afterErr := f.Stat()
	closeErr := f.Close()
	if readErr != nil {
		return nil, nil, nil, readErr
	}
	if afterErr != nil {
		return nil, nil, nil, afterErr
	}
	if closeErr != nil {
		return nil, nil, nil, closeErr
	}
	if int64(len(data)) != before.Size() {
		return nil, nil, nil, fmt.Errorf("source file size changed while reading: %s", name)
	}
	return data, before, after, nil
}

func verifyDirectorySnapshot(ctx context.Context, source string, expected map[string][32]byte, excluded ...string) error {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	seen := make(map[string]struct{}, len(expected))
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
		name = path.Clean(name)
		if !utf8.ValidString(name) || !validImportPath(name) {
			return fmt.Errorf("invalid path appeared during import: %s", name)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink appeared during import: %s", name)
		}
		if skipExplicitImportPath(name, excluded) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		want, ok := expected[name]
		if !ok {
			return fmt.Errorf("new file appeared during import: %s", name)
		}
		data, _, _, err := readRootFile(ctx, root, name)
		if err != nil {
			return err
		}
		if sha256.Sum256(data) != want {
			return fmt.Errorf("file contents changed during import: %s", name)
		}
		seen[name] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("source file set changed during import")
	}
	return nil
}

func readZipSource(ctx context.Context, source string, excluded ...string) ([]importEntry, func() error, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxArchiveBytes {
		return nil, nil, fmt.Errorf("ZIP archive must be a bounded regular file")
	}
	f, err := os.Open(source)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameFileSnapshot(info, opened) {
		return nil, nil, fmt.Errorf("ZIP archive changed while opening")
	}
	beforeHash, err := hashFile(ctx, f, maxArchiveBytes)
	if err != nil {
		return nil, nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	archive, err := zip.NewReader(f, info.Size())
	if err != nil {
		return nil, nil, fmt.Errorf("read ZIP archive: %w", err)
	}
	if len(archive.File) > maxArchiveEntries {
		return nil, nil, fmt.Errorf("ZIP entry count limit exceeded")
	}
	entries := make([]importEntry, 0, len(archive.File))
	fileCount, dirCount, total := 0, 0, uint64(0)
	for _, zf := range archive.File {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		name, isDir, err := zipEntryName(zf)
		if err != nil {
			return nil, nil, err
		}
		if name == "" {
			continue
		}
		if !validImportPath(name) {
			return nil, nil, fmt.Errorf("invalid ZIP path %q", name)
		}
		mode := zf.Mode()
		if mode&os.ModeSymlink != 0 || (isDir && !mode.IsDir() && mode.Type() != 0) || (!isDir && !mode.IsRegular()) {
			return nil, nil, fmt.Errorf("ZIP links and special files are forbidden: %s", name)
		}
		if skipExplicitImportPath(name, excluded) {
			continue
		}
		if isDir {
			dirCount++
			if dirCount > maxImportDirs {
				return nil, nil, fmt.Errorf("ZIP directory limit exceeded")
			}
			entries = append(entries, importEntry{name: name, dir: true})
			continue
		}
		fileCount++
		if fileCount > maxImportFiles || zf.UncompressedSize64 > MaxFileBytes {
			return nil, nil, fmt.Errorf("ZIP file limit exceeded")
		}
		total += zf.UncompressedSize64
		if total > MaxBundleBytes {
			return nil, nil, fmt.Errorf("ZIP expanded bundle limit exceeded")
		}
		r, err := zf.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("open ZIP entry %q: %w", name, err)
		}
		data, readErr := readBounded(ctx, r, MaxFileBytes)
		closeErr := r.Close()
		if readErr != nil {
			return nil, nil, fmt.Errorf("read ZIP entry %q: %w", name, readErr)
		}
		if closeErr != nil {
			return nil, nil, fmt.Errorf("close ZIP entry %q: %w", name, closeErr)
		}
		if uint64(len(data)) != zf.UncompressedSize64 {
			return nil, nil, fmt.Errorf("ZIP entry size mismatch: %s", name)
		}
		entries = append(entries, importEntry{name: name, data: data})
	}
	if fileCount == 0 {
		return nil, nil, fmt.Errorf("ZIP archive contains no files")
	}
	verify := func() error {
		current, err := os.Lstat(source)
		if err != nil || !sameFileSnapshot(info, current) || current.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("ZIP archive changed during import")
		}
		check, err := os.Open(source)
		if err != nil {
			return err
		}
		defer check.Close()
		opened, err := check.Stat()
		if err != nil || !sameFileSnapshot(info, opened) {
			return fmt.Errorf("ZIP archive changed during import")
		}
		got, err := hashFile(ctx, check, maxArchiveBytes)
		if err != nil {
			return err
		}
		if got != beforeHash {
			return fmt.Errorf("ZIP archive contents changed during import")
		}
		return nil
	}
	return entries, verify, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func readTarSource(ctx context.Context, source string, excluded ...string) ([]importEntry, func() error, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 {
		return nil, nil, fmt.Errorf("TAR archive must be a regular file")
	}
	compressed := strings.HasSuffix(strings.ToLower(source), ".gz") || strings.EqualFold(filepath.Ext(source), ".tgz")
	maxInput := int64(maxTarExpanded)
	if compressed {
		maxInput = maxArchiveBytes
	}
	if info.Size() > maxInput {
		return nil, nil, fmt.Errorf("TAR archive exceeds compressed input limit")
	}
	f, err := os.Open(source)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameFileSnapshot(info, opened) {
		return nil, nil, fmt.Errorf("TAR archive changed while opening")
	}
	beforeHash, err := hashFile(ctx, f, maxInput)
	if err != nil {
		return nil, nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	var stream io.Reader = f
	var gz *gzip.Reader
	if compressed {
		gz, err = gzip.NewReader(f)
		if err != nil {
			return nil, nil, fmt.Errorf("open gzip TAR: %w", err)
		}
		stream = gz
	}
	limited := &io.LimitedReader{R: stream, N: int64(maxTarExpanded) + 1}
	counted := &countingReader{r: limited}
	tr := tar.NewReader(counted)
	entries := make([]importEntry, 0)
	fileCount, dirCount, total := 0, 0, int64(0)
	entryCount := 0
	for {
		if err := ctx.Err(); err != nil {
			if gz != nil {
				_ = gz.Close()
			}
			return nil, nil, err
		}
		header, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			if gz != nil {
				_ = gz.Close()
			}
			return nil, nil, fmt.Errorf("read TAR header: %w", nextErr)
		}
		entryCount++
		if entryCount > maxArchiveEntries {
			if gz != nil {
				_ = gz.Close()
			}
			return nil, nil, fmt.Errorf("TAR entry count limit exceeded")
		}
		name := header.Name
		if strings.HasPrefix(name, "/") {
			return nil, nil, fmt.Errorf("absolute TAR paths are forbidden")
		}
		if strings.HasSuffix(name, "/") {
			name = strings.TrimSuffix(name, "/")
		}
		if name == "" || name == "." {
			if header.Typeflag == tar.TypeDir && (header.Name == "." || header.Name == "./") {
				continue
			}
			return nil, nil, fmt.Errorf("empty TAR file path")
		}
		if !utf8.ValidString(name) || !validImportPath(name) {
			return nil, nil, fmt.Errorf("invalid TAR path %q", name)
		}
		isDir := header.Typeflag == tar.TypeDir
		if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, nil, fmt.Errorf("TAR links and special files are forbidden: %s", name)
		}
		if skipExplicitImportPath(name, excluded) {
			if header.Size > 0 {
				if _, err := io.Copy(io.Discard, tr); err != nil {
					return nil, nil, err
				}
			}
			continue
		}
		if isDir {
			if header.Size != 0 {
				return nil, nil, fmt.Errorf("TAR directory has contents: %s", name)
			}
			dirCount++
			if dirCount > maxImportDirs {
				return nil, nil, fmt.Errorf("TAR directory limit exceeded")
			}
			entries = append(entries, importEntry{name: name, dir: true})
			continue
		}
		fileCount++
		if header.Size < 0 || header.Size > MaxFileBytes || fileCount > maxImportFiles {
			return nil, nil, fmt.Errorf("TAR file limit exceeded")
		}
		total += header.Size
		if total > MaxBundleBytes {
			return nil, nil, fmt.Errorf("TAR expanded file size limit exceeded")
		}
		data, err := readBounded(ctx, tr, MaxFileBytes)
		if err != nil {
			return nil, nil, fmt.Errorf("read TAR file %q: %w", name, err)
		}
		if int64(len(data)) != header.Size {
			return nil, nil, fmt.Errorf("TAR file size mismatch: %s", name)
		}
		entries = append(entries, importEntry{name: name, data: data})
	}
	if _, err := io.Copy(io.Discard, counted); err != nil {
		if gz != nil {
			_ = gz.Close()
		}
		return nil, nil, fmt.Errorf("finish TAR stream: %w", err)
	}
	if counted.n > int64(maxTarExpanded) {
		if gz != nil {
			_ = gz.Close()
		}
		return nil, nil, fmt.Errorf("TAR expanded metadata size limit exceeded")
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			return nil, nil, fmt.Errorf("close gzip TAR: %w", err)
		}
	}
	if fileCount == 0 {
		return nil, nil, fmt.Errorf("TAR archive contains no files")
	}
	verify := func() error {
		current, err := os.Lstat(source)
		if err != nil || !sameFileSnapshot(info, current) || current.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("TAR archive changed during import")
		}
		check, err := os.Open(source)
		if err != nil {
			return err
		}
		defer check.Close()
		opened, err := check.Stat()
		if err != nil || !sameFileSnapshot(info, opened) {
			return fmt.Errorf("TAR archive changed during import")
		}
		got, err := hashFile(ctx, check, maxInput)
		if err != nil {
			return err
		}
		if got != beforeHash {
			return fmt.Errorf("TAR archive contents changed during import")
		}
		return nil
	}
	return entries, verify, nil
}

func zipEntryName(zf *zip.File) (string, bool, error) {
	name := zf.Name
	if !utf8.ValidString(name) || strings.Contains(name, "\\") || strings.ContainsRune(name, '\x00') {
		return "", false, fmt.Errorf("invalid ZIP entry path")
	}
	if strings.HasPrefix(name, "/") {
		return "", false, fmt.Errorf("absolute ZIP paths are forbidden")
	}
	isDir := strings.HasSuffix(name, "/") || zf.Mode().IsDir()
	name = strings.TrimSuffix(name, "/")
	if name == "" || name == "." {
		if isDir && (zf.Name == "." || zf.Name == "./") {
			return "", true, nil
		}
		return "", false, fmt.Errorf("empty ZIP file path")
	}
	return name, isDir, nil
}

func readBounded(ctx context.Context, r io.Reader, limit int) ([]byte, error) {
	data := make([]byte, 0, min(limit, 32<<10))
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if len(data)+n > limit {
				return nil, fmt.Errorf("file exceeds %d byte limit", limit)
			}
			data = append(data, buf[:n]...)
		}
		if err == io.EOF {
			return data, nil
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
}

func hashFile(ctx context.Context, f *os.File, limit int64) ([32]byte, error) {
	var zero [32]byte
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return zero, err
	}
	h := sha256.New()
	buf := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		n, err := f.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > limit {
				return zero, fmt.Errorf("archive exceeds %d byte limit", limit)
			}
			_, _ = h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return zero, err
		}
		if n == 0 {
			return zero, io.ErrNoProgress
		}
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func sameFileSnapshot(a, b fs.FileInfo) bool {
	return a != nil && b != nil && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func shouldSkipImportPath(name string) bool {
	for _, component := range strings.Split(name, "/") {
		if strings.EqualFold(component, ".git") || strings.EqualFold(component, ".cache") || strings.EqualFold(component, "__MACOSX") || strings.HasPrefix(component, "._") {
			return true
		}
	}
	return false
}

func validImportPath(name string) bool {
	if !ValidPath(name) || !utf8.ValidString(name) || len(name) > maxImportPathUnits*4 {
		return false
	}
	units := 0
	for _, component := range strings.Split(name, "/") {
		if component == "" || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") || strings.ContainsAny(component, `<>"|?*`) {
			return false
		}
		componentUnits := len(utf16.Encode([]rune(component)))
		units += componentUnits + 1
		if componentUnits > 255 {
			return false
		}
		for _, r := range component {
			if r < 0x20 || r == 0x7f {
				return false
			}
		}
		base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
		base = strings.TrimRight(base, " .")
		if isWindowsDeviceName(base) {
			return false
		}
	}
	return units <= maxImportPathUnits
}

func isWindowsDeviceName(name string) bool {
	switch name {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$", "CONIN$", "CONOUT$":
		return true
	}
	if len(name) == 4 && (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) {
		last := name[3]
		return last >= '1' && last <= '9'
	}
	switch name {
	case "COM¹", "COM²", "COM³", "LPT¹", "LPT²", "LPT³":
		return true
	}
	return false
}

func normalizeImportRoot(entries []importEntry) ([]importEntry, error) {
	top := ""
	allNested := len(entries) > 0
	hasGoMod := false
	for _, entry := range entries {
		parts := strings.Split(entry.name, "/")
		if len(parts) == 1 && entry.dir {
			if top == "" {
				top = parts[0]
			} else if !strings.EqualFold(top, parts[0]) {
				allNested = false
				break
			}
			continue
		}
		if len(parts) < 2 {
			allNested = false
			break
		}
		if top == "" {
			top = parts[0]
		} else if !strings.EqualFold(top, parts[0]) {
			allNested = false
			break
		}
		if len(parts) == 2 && strings.EqualFold(parts[1], "go.mod") && !entry.dir {
			hasGoMod = true
		}
	}
	if !allNested || !hasGoMod {
		return entries, nil
	}
	stripped := make([]importEntry, 0, len(entries))
	for _, entry := range entries {
		parts := strings.Split(entry.name, "/")
		if !strings.EqualFold(parts[0], top) {
			return nil, fmt.Errorf("inconsistent repository root prefix")
		}
		if len(parts) == 1 {
			continue
		}
		entry.name = strings.Join(parts[1:], "/")
		if !validImportPath(entry.name) {
			return nil, fmt.Errorf("invalid path after repository root normalization")
		}
		stripped = append(stripped, entry)
	}
	sortImportEntries(stripped)
	return stripped, nil
}

func validateImportTree(entries []importEntry) error {
	nodes := make(map[string]importNode, len(entries))
	explicit := make(map[string]struct{}, len(entries))
	files, dirs, total := 0, 0, 0
	for _, entry := range entries {
		if !validImportPath(entry.name) {
			return fmt.Errorf("invalid import path %q", entry.name)
		}
		canonical := strings.ToLower(entry.name)
		if _, duplicate := explicit[canonical]; duplicate {
			return fmt.Errorf("duplicate or case-colliding path %q", entry.name)
		}
		explicit[canonical] = struct{}{}
		parts := strings.Split(entry.name, "/")
		for i := 1; i < len(parts); i++ {
			parent := strings.ToLower(strings.Join(parts[:i], "/"))
			if existing, ok := nodes[parent]; ok && !existing.dir {
				return fmt.Errorf("file/directory collision at %q", entry.name)
			}
			if _, ok := nodes[parent]; !ok {
				nodes[parent] = importNode{dir: true}
				dirs++
			}
		}
		if existing, ok := nodes[canonical]; ok {
			if !entry.dir || !existing.dir || existing.explicit {
				return fmt.Errorf("file/directory collision at %q", entry.name)
			}
			existing.explicit = true
			nodes[canonical] = existing
		} else {
			nodes[canonical] = importNode{dir: entry.dir, explicit: true}
			if entry.dir {
				dirs++
			}
		}
		if entry.dir {
			if dirs > maxImportDirs {
				return fmt.Errorf("import directory count limit exceeded")
			}
			continue
		}
		files++
		if files > maxImportFiles || len(entry.data) > MaxFileBytes {
			return fmt.Errorf("import file count or size limit exceeded")
		}
		total += len(entry.data)
		if total > MaxBundleBytes {
			return fmt.Errorf("import expanded size limit exceeded")
		}
	}
	if files == 0 {
		return fmt.Errorf("source contains no files")
	}
	return nil
}

func sortImportEntries(entries []importEntry) {
	sort.Slice(entries, func(i, j int) bool { return strings.ToLower(entries[i].name) < strings.ToLower(entries[j].name) })
}

func skipExplicitImportPath(name string, excluded []string) bool {
	if shouldSkipImportPath(name) {
		return true
	}
	for _, p := range excluded {
		if name == p || strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}
