package dataset

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
)

const (
	maxScanDirectories = 100_000
	maxScanDepth       = 24
	maxOuterEntries    = 3000
	maxOuterExpanded   = 128 << 20
	maxOuterCompressed = 64 << 20
)

type sourceCandidate struct {
	path          string
	member        string
	prio          int
	rawSHA        string
	memberSHA     string
	promptMembers []string
	compatible    bool
	note          string
}

type tarFile struct {
	name string
	data []byte
}

type tarIndex struct {
	files []tarFile
	dirs  []string
}

// Scan discovers task packages below root. The E3 adapter is intentionally
// conservative: it recognizes explicit prompt/rules boundaries and records
// ambiguous or unscorable packages instead of selecting a grader or answer.
func Scan(ctx context.Context, root, adapter string) (Catalog, error) {
	if ctx == nil {
		return Catalog{}, fmt.Errorf("context is required")
	}
	if adapter != "e3" {
		return Catalog{}, fmt.Errorf("unsupported dataset adapter %q", adapter)
	}
	absRoot, err := realRoot(root)
	if err != nil {
		return Catalog{}, err
	}
	dirs, err := walkDirectories(ctx, absRoot)
	if err != nil {
		return Catalog{}, err
	}
	c := Catalog{SchemaVersion: CatalogSchemaVersion, Name: filepath.Base(absRoot), Root: absRoot, Adapter: adapter}
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return Catalog{}, err
		}
		item, ok, err := scanPackage(ctx, absRoot, dir)
		if err != nil {
			return Catalog{}, fmt.Errorf("scan package %s: %w", relative(absRoot, dir), err)
		}
		if ok {
			c.Items = append(c.Items, item)
			if len(c.Items) > MaxCatalogItems {
				return Catalog{}, fmt.Errorf("dataset contains more than %d task packages", MaxCatalogItems)
			}
		}
	}
	sort.Slice(c.Items, func(i, j int) bool { return c.Items[i].Key < c.Items[j].Key })
	counts := make(map[string]int)
	for _, item := range c.Items {
		counts[item.TaskID]++
	}
	for i := range c.Items {
		if counts[c.Items[i].TaskID] > 1 {
			c.Items[i].Notes = append(c.Items[i].Notes, "duplicate task_id found; package key remains distinct")
		}
	}
	if err := validateCatalog(c); err != nil {
		return Catalog{}, err
	}
	return c, nil
}

func walkDirectories(ctx context.Context, root string) ([]string, error) {
	dirs := make([]string, 0, 256)
	visited := 0
	err := filepath.WalkDir(root, func(name string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		visited++
		if visited > maxScanDirectories {
			return fmt.Errorf("dataset directory scan limit exceeded")
		}
		rel, _ := filepath.Rel(root, name)
		depth := 0
		if rel != "." {
			depth = len(strings.Split(filepath.ToSlash(rel), "/"))
		}
		if depth > maxScanDepth {
			return filepath.SkipDir
		}
		if name != root && ignoredScanDir(d.Name()) && !potentialPackageRoot(name) {
			return filepath.SkipDir
		}
		dirs = append(dirs, name)
		if name != root && potentialPackageRoot(name) {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(dirs, func(i, j int) bool { return filepath.ToSlash(dirs[i]) < filepath.ToSlash(dirs[j]) })
	return dirs, nil
}

func ignoredScanDir(name string) bool {
	lower := strings.ToLower(name)
	if lower == "__macosx" || strings.HasPrefix(name, "._") || strings.Contains(lower, "cache") || strings.Contains(lower, "提交材料") || strings.Contains(lower, "作答材料") || strings.Contains(lower, "过期") {
		return true
	}
	switch lower {
	case ".git", ".tmp", ".cache", "cache", "gocache", "gomodcache", "node_modules", "vendor", "target", "dist":
		return true
	}
	return strings.HasPrefix(lower, ".work") || strings.HasPrefix(lower, ".b") && strings.Contains(lower, "work")
}

func potentialPackageRoot(dir string) bool {
	if hasBoundaryNames(dir) {
		return true
	}
	material := filepath.Join(dir, "01-任务执行材料")
	st, err := os.Lstat(material)
	return err == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0 && hasBoundaryNames(material)
}

func hasBoundaryNames(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	formal, rules := false, false
	for _, e := range entries {
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		switch strings.ToLower(e.Name()) {
		case "01-正式题目.md", "题目.md":
			formal = true
		case "02-作答执行规则.md", "readme-做题说明.md":
			rules = true
		}
	}
	return formal && rules
}

func scanPackage(ctx context.Context, root, dir string) (Item, bool, error) {
	item := Item{State: "candidate", ExpectedSHA256: make(map[string]string), Metadata: map[string]string{"adapter": "e3"}}
	packageRel := relative(root, dir)
	if packageRel == "" {
		packageRel = "."
	}
	item.PackagePath = packageRel
	execDir := dir
	materialDir := filepath.Join(dir, "01-任务执行材料")
	if info, err := os.Lstat(materialDir); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		execDir = materialDir
	}
	promptFiles := collectPromptFiles(root, dir, execDir)
	if !hasTaskBoundary(promptFiles) {
		return Item{}, false, nil
	}
	item.PromptFiles = promptFiles
	item.TaskID, item.Metadata["task_id_source"] = discoverTaskID(root, dir, promptFiles)
	item.Metadata["package_format"] = "e3"

	sources, err := discoverSources(ctx, root, dir, execDir)
	if err != nil {
		item.State = "incompatible"
		item.Notes = append(item.Notes, "source inspection failed: "+trimNote(err.Error()))
	} else if len(sources) == 0 {
		item.State = "incompatible"
		item.Notes = append(item.Notes, "no supported Go source directory/archive found")
	} else {
		sort.Slice(sources, func(i, j int) bool {
			if sources[i].prio != sources[j].prio {
				return sources[i].prio > sources[j].prio
			}
			return sources[i].path < sources[j].path
		})
		best := sources[0].prio
		var winners []sourceCandidate
		for _, source := range sources {
			if source.prio == best {
				winners = append(winners, source)
			}
		}
		if len(winners) > 1 {
			item.State = "ambiguous"
			item.Notes = append(item.Notes, "multiple equally preferred source snapshots found; operator selection required")
		}
		chosen := winners[0]
		item.Source = chosen.path
		item.SourceMember = chosen.member
		item.Metadata["source_type"] = sourceType(chosen.path)
		item.Metadata["source_sha256"] = chosen.rawSHA
		if chosen.rawSHA != "" {
			item.ExpectedSHA256[chosen.path] = chosen.rawSHA
		}
		if chosen.member != "" {
			item.Metadata["source_member"] = chosen.member
			if chosen.memberSHA != "" {
				item.ExpectedSHA256[chosen.path+"!"+chosen.member] = chosen.memberSHA
			}
		}
		item.PromptMembers = append(item.PromptMembers, chosen.promptMembers...)
		if !chosen.compatible {
			item.State = "incompatible"
			if chosen.note != "" {
				item.Notes = append(item.Notes, trimNote(chosen.note))
			}
		}
	}

	grader := filepath.Join(dir, "02-内部评测材料", "grader")
	graderInfo, graderErr := os.Lstat(grader)
	if graderErr == nil && graderInfo.IsDir() && graderInfo.Mode()&os.ModeSymlink == 0 {
		item.GraderDir = relative(root, grader)
		entry := filepath.Join(grader, "grade.py")
		if info, err := os.Lstat(entry); err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			item.GraderEntry = "grade.py"
		}
	}
	if item.State == "candidate" && (item.GraderDir == "" || item.GraderEntry == "") {
		item.State = "missing_grader"
		item.Notes = append(item.Notes, "trusted grader entrypoint was not found; source can be run but is not locally scoreable")
	}
	if item.TaskID == "" || item.Source == "" {
		// Keep malformed task candidates visible for operator review.
		if item.TaskID == "" {
			item.TaskID = fallbackTaskID(filepath.Base(dir))
			item.Notes = append(item.Notes, "task_id inferred from package directory")
		}
		if item.Source == "" {
			item.State = "incompatible"
		}
	}
	keySeed := item.PackagePath + "\x00" + item.Source + "\x00" + item.SourceMember + "\x00" + item.Metadata["source_sha256"]
	keySum := sha256.Sum256([]byte(keySeed))
	item.Key = "e3-" + hex.EncodeToString(keySum[:8])
	if err := collectInputHashes(ctx, root, &item); err != nil {
		item.State = "incompatible"
		item.Notes = append(item.Notes, "protected input inspection failed: "+trimNote(err.Error()))
	}
	return item, true, nil
}

func collectPromptFiles(root, packageDir, execDir string) []string {
	var paths []string
	seen := map[string]struct{}{}
	// Numbered task material is an explicit disclosure boundary. A package-level
	// README in that layout may contain grader details and is deliberately omitted.
	bases := []string{packageDir}
	if execDir != packageDir {
		bases = []string{execDir}
	}
	for _, base := range bases {
		for _, name := range []string{"01-正式题目.md", "题目.md", "02-作答执行规则.md", "README-做题说明.md"} {
			candidate := filepath.Join(base, name)
			st, err := os.Lstat(candidate)
			if err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 || st.Size() > MaxPromptBytes {
				continue
			}
			rel := relative(root, candidate)
			if _, ok := seen[rel]; ok {
				continue
			}
			seen[rel] = struct{}{}
			paths = append(paths, rel)
		}
	}
	sort.Strings(paths)
	if len(paths) > MaxPromptFiles {
		return paths[:MaxPromptFiles]
	}
	return paths
}

func hasTaskBoundary(paths []string) bool {
	formal, rules := false, false
	for _, p := range paths {
		base := strings.ToLower(filepath.Base(p))
		if base == "01-正式题目.md" || base == "题目.md" {
			formal = true
		}
		if base == "02-作答执行规则.md" || base == "readme-做题说明.md" {
			rules = true
		}
	}
	return formal && rules
}

func discoverTaskID(root, dir string, prompts []string) (string, string) {
	for _, name := range []string{filepath.Join(dir, "02-内部评测材料", "meta.json"), filepath.Join(dir, "meta.json")} {
		data, err := readBoundedFile(name, 1<<20)
		if err != nil || !utf8.Valid(data) {
			continue
		}
		var meta struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(data, &meta) == nil && taskPattern.MatchString(meta.TaskID) {
			return meta.TaskID, "meta.json"
		}
	}
	for _, prompt := range prompts {
		b, err := readBoundedFile(filepath.Join(root, filepath.FromSlash(prompt)), MaxPromptBytes)
		if err != nil || !utf8.Valid(b) {
			continue
		}
		if match := taskIDLine.FindSubmatch(b); len(match) == 2 && taskPattern.Match(match[1]) {
			return string(match[1]), "prompt"
		}
		if match := task013ID.FindSubmatch(b); len(match) == 2 && taskPattern.Match(match[1]) {
			return "Task013-" + string(match[1]), "prompt"
		}
	}
	return "", "directory"
}

func fallbackTaskID(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
	}
	id := strings.Trim(b.String(), "-._:")
	if !taskPattern.MatchString(id) {
		return "unknown-task"
	}
	return id
}

func discoverSources(ctx context.Context, root, packageDir, execDir string) ([]sourceCandidate, error) {
	var found []sourceCandidate
	seen := map[string]struct{}{}
	for _, base := range uniquePaths(execDir, packageDir) {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if entry.Type()&os.ModeSymlink != 0 || ignoredEntry(entry.Name()) {
				continue
			}
			full := filepath.Join(base, entry.Name())
			if entry.IsDir() && (entry.Name() == "repo" || entry.Name() == "code") {
				if hasGoMod(full) {
					digest, err := directoryDigest(ctx, full)
					candidate := sourceCandidate{path: relative(root, full), prio: 1, compatible: err == nil}
					if err == nil {
						candidate.rawSHA = digest
					} else {
						candidate.note = err.Error()
					}
					addSource(&found, seen, candidate)
				}
				continue
			}
			if !entry.Type().IsRegular() && !entry.IsDir() {
				continue
			}
			if entry.IsDir() {
				continue
			}
			if !looksLikeSourceArchive(entry.Name()) {
				continue
			}
			st, err := os.Lstat(full)
			if err != nil || !st.Mode().IsRegular() || st.Size() > maxOuterCompressed {
				continue
			}
			candidate := sourceCandidate{path: relative(root, full), prio: archivePriority(entry.Name()), compatible: true}
			candidate.rawSHA, err = hashFileBounded(ctx, full, maxOuterCompressed)
			if err != nil {
				candidate.compatible = false
				candidate.note = err.Error()
			}
			if strings.HasSuffix(strings.ToLower(entry.Name()), ".tar.gz") || strings.HasSuffix(strings.ToLower(entry.Name()), ".tgz") || strings.EqualFold(filepath.Ext(entry.Name()), ".tar") {
				idx, err := inspectTar(ctx, full, nil)
				if err != nil {
					candidate.compatible = false
					candidate.note = err.Error()
					addSource(&found, seen, candidate)
					continue
				}
				member, promptMembers, err := chooseSnapshot(idx)
				if err != nil {
					candidate.compatible = false
					candidate.note = err.Error()
					addSource(&found, seen, candidate)
					continue
				}
				candidate.member = member
				candidate.promptMembers = promptMembers
				candidate.memberSHA = snapshotDigest(idx, member)
				if member != "" && isSupportedArchive(member) {
					var nested []byte
					for _, f := range idx.files {
						if f.name == member {
							nested = f.data
							break
						}
					}
					innerPrompts, err := inspectNestedArchive(ctx, member, nested)
					if err != nil {
						candidate.compatible = false
						candidate.note = "nested source archive: " + err.Error()
					} else {
						candidate.promptMembers = nil
						for _, prompt := range innerPrompts {
							candidate.promptMembers = append(candidate.promptMembers, member+"!"+prompt)
						}
					}
				}
				if !snapshotWithinLimits(idx, member) {
					candidate.compatible = false
					candidate.note = "selected source snapshot exceeds file/count/bundle import limits"
				}
			}
			if strings.EqualFold(filepath.Ext(entry.Name()), ".zip") {
				_, candidate.promptMembers, err = inspectZipMeta(ctx, full)
				if err != nil {
					candidate.compatible = false
					candidate.note = err.Error()
				}
			}
			addSource(&found, seen, candidate)
		}
	}
	if hasGoMod(packageDir) {
		digest, err := directoryDigest(ctx, packageDir)
		candidate := sourceCandidate{path: relative(root, packageDir), prio: 1, compatible: err == nil}
		if err == nil {
			candidate.rawSHA = digest
		} else {
			candidate.note = err.Error()
		}
		addSource(&found, seen, candidate)
	}
	return found, nil
}

func addSource(out *[]sourceCandidate, seen map[string]struct{}, source sourceCandidate) {
	key := source.path + "!" + source.member
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	*out = append(*out, source)
}

func looksLikeSourceArchive(name string) bool {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "提交") || strings.Contains(lower, "答案") || strings.Contains(lower, "candidate") || strings.Contains(lower, "solution") || strings.Contains(lower, "golden") || strings.Contains(lower, "submission") {
		return false
	}
	return lower == "code.zip" || strings.Contains(lower, "任务包") || strings.Contains(lower, "snapshot") || lower == "repo.zip" || lower == "repo.tar.gz" || strings.HasPrefix(lower, "repo.")
}

func archivePriority(name string) int {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return 3
	case strings.HasSuffix(lower, ".tar"):
		return 2
	default:
		return 0
	}
}

func sourceType(name string) string {
	lower := strings.ToLower(name)
	if !strings.Contains(lower, ".zip") && !strings.Contains(lower, ".tar") {
		return "directory"
	}
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return "zip"
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(lower, ".tar"):
		return "tar"
	default:
		return "unknown"
	}
}

func inspectZip(ctx context.Context, file string) error {
	_, _, err := inspectZipMeta(ctx, file)
	return err
}

func inspectNestedArchive(ctx context.Context, name string, data []byte) ([]string, error) {
	if len(data) == 0 || len(data) > maxOuterCompressed {
		return nil, fmt.Errorf("nested archive size is invalid")
	}
	dir, err := os.MkdirTemp("", "agenteval-dataset-index-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, filepath.Base(filepath.FromSlash(name)))
	if err := os.WriteFile(file, data, 0600); err != nil {
		return nil, err
	}
	if strings.HasSuffix(strings.ToLower(name), ".zip") {
		_, prompts, err := inspectZipMeta(ctx, file)
		return prompts, err
	}
	idx, err := inspectTar(ctx, file, nil)
	if err != nil {
		return nil, err
	}
	_, prompts, err := chooseSnapshot(idx)
	if err != nil {
		return nil, err
	}
	return prompts, nil
}

func inspectZipMeta(ctx context.Context, file string) (string, []string, error) {
	st, err := os.Lstat(file)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() || st.Size() > maxOuterCompressed {
		return "", nil, fmt.Errorf("ZIP source exceeds archive policy")
	}
	zr, err := zip.OpenReader(file)
	if err != nil {
		return "", nil, err
	}
	defer zr.Close()
	if len(zr.File) > maxOuterEntries {
		return "", nil, fmt.Errorf("ZIP entry limit exceeded")
	}
	var expanded uint64
	files := 0
	seen := make(map[string]struct{}, len(zr.File))
	var mods []string
	fileNames := make(map[string]struct{}, len(zr.File))
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		name, dir, err := cleanArchiveName(f.Name)
		if err != nil {
			return "", nil, err
		}
		if !dir && (f.Mode()&os.ModeSymlink != 0 || !f.Mode().IsRegular()) {
			return "", nil, fmt.Errorf("ZIP links/special files are forbidden")
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return "", nil, fmt.Errorf("duplicate ZIP destination")
		}
		seen[key] = struct{}{}
		if !dir {
			if f.UncompressedSize64 > uint64(task.MaxFileBytes) {
				return "", nil, fmt.Errorf("ZIP file exceeds per-file import limit")
			}
			files++
			if files > 1000 {
				return "", nil, fmt.Errorf("ZIP file count exceeds import limit")
			}
			fileNames[name] = struct{}{}
			if path.Base(name) == "go.mod" {
				mods = append(mods, path.Dir(name))
			}
		}
		expanded += f.UncompressedSize64
		if expanded > uint64(task.MaxBundleBytes) {
			return "", nil, fmt.Errorf("ZIP expanded limit exceeded")
		}
	}
	root, err := chooseModuleRoot(mods)
	if err != nil {
		return "", nil, err
	}
	if root != "" {
		for name := range fileNames {
			if !strings.HasPrefix(name, root+"/") {
				return "", nil, fmt.Errorf("ZIP source root is mixed with unrelated top-level files")
			}
		}
	}
	var prompts []string
	for name := range fileNames {
		if root != "" && !strings.HasPrefix(name, root+"/") {
			continue
		}
		rel := name
		if root != "" {
			rel = strings.TrimPrefix(name, root+"/")
		}
		base := strings.ToLower(path.Base(rel))
		if base == "instruction.md" && path.Dir(rel) == "." {
			prompts = append(prompts, name)
		}
	}
	sort.Strings(prompts)
	return root, prompts, nil
}

func chooseModuleRoot(mods []string) (string, error) {
	if len(mods) == 0 {
		return "", fmt.Errorf("archive contains no Go module")
	}
	sort.Slice(mods, func(i, j int) bool {
		if strings.Count(mods[i], "/") != strings.Count(mods[j], "/") {
			return strings.Count(mods[i], "/") < strings.Count(mods[j], "/")
		}
		return mods[i] < mods[j]
	})
	depth := strings.Count(mods[0], "/")
	var roots []string
	for _, mod := range mods {
		if strings.Count(mod, "/") == depth {
			roots = append(roots, mod)
		}
	}
	roots = uniqueStrings(roots)
	if len(roots) != 1 {
		return "", fmt.Errorf("archive has multiple equally shallow Go module roots")
	}
	if roots[0] == "." {
		return "", nil
	}
	return roots[0], nil
}

func readBoundedFile(name string, limit int64) ([]byte, error) {
	st, err := os.Lstat(name)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() || st.Size() < 0 || st.Size() > limit {
		return nil, fmt.Errorf("file is missing, linked, or exceeds limit")
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(st, opened) {
		_ = f.Close()
		return nil, fmt.Errorf("file changed while opening")
	}
	b, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("file exceeds limit")
	}
	return b, nil
}

func relative(root, name string) string {
	rel, err := filepath.Rel(root, name)
	if err != nil {
		return ""
	}
	if rel == "." {
		return "."
	}
	return filepath.ToSlash(rel)
}

func uniquePaths(paths ...string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		clean := filepath.Clean(p)
		if _, ok := seen[clean]; !ok {
			seen[clean] = struct{}{}
			out = append(out, clean)
		}
	}
	return out
}

func ignoredEntry(name string) bool {
	return strings.EqualFold(name, "__MACOSX") || strings.HasPrefix(name, "._") || ignoredScanDir(name)
}

func hasGoMod(dir string) bool {
	p := filepath.Join(dir, "go.mod")
	st, err := os.Lstat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&os.ModeSymlink == 0
}

func trimNote(s string) string {
	if len(s) > 800 {
		return s[:800]
	}
	return s
}

var (
	taskIDLine = regexp.MustCompile(`(?im)^\s*task[_ -]?id\s*[:=]\s*([A-Za-z0-9][A-Za-z0-9_-]{0,79})\b`)
	task013ID  = regexp.MustCompile(`(?i)\bTask013-([A-Za-z0-9][A-Za-z0-9_-]{0,79})\b`)
)

func collectInputHashes(ctx context.Context, root string, item *Item) error {
	for _, rel := range item.PromptFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		full, err := Resolve(root, rel)
		if err != nil {
			return err
		}
		hash, err := hashFileBounded(ctx, full, task.MaxFileBytes)
		if err != nil {
			return fmt.Errorf("prompt %s: %w", rel, err)
		}
		item.ExpectedSHA256[rel] = hash
	}
	if item.GraderDir == "" {
		return nil
	}
	grader, err := Resolve(root, item.GraderDir)
	if err != nil {
		return fmt.Errorf("grader directory: %w", err)
	}
	rootFS, err := os.OpenRoot(grader)
	if err != nil {
		return err
	}
	defer rootFS.Close()
	count := 0
	return fs.WalkDir(rootFS.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in grader directory")
		}
		if d.IsDir() {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() || st.Size() < 0 || st.Size() > task.MaxFileBytes {
			return fmt.Errorf("grader file exceeds protected-input limits")
		}
		count++
		if count > 1000 {
			return fmt.Errorf("grader file count exceeds protected-input limit")
		}
		rel := filepath.ToSlash(filepath.Join(item.GraderDir, filepath.FromSlash(name)))
		full, err := Resolve(root, rel)
		if err != nil {
			return err
		}
		hash, err := hashFileBounded(ctx, full, task.MaxFileBytes)
		if err != nil {
			return err
		}
		item.ExpectedSHA256[rel] = hash
		return nil
	})
}

// inspectTar validates every outer member and stores only regular-file payloads
// after rejecting links, duplicate normalized paths, and oversized archives.
func inspectTar(ctx context.Context, file string, selected map[string]bool) (tarIndex, error) {
	var idx tarIndex
	st, err := os.Lstat(file)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() || st.Size() > maxOuterCompressed {
		return idx, fmt.Errorf("TAR source exceeds archive policy")
	}
	f, err := os.Open(file)
	if err != nil {
		return idx, err
	}
	defer f.Close()
	var input io.Reader = f
	if strings.HasSuffix(strings.ToLower(file), ".gz") || strings.HasSuffix(strings.ToLower(file), ".tgz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return idx, err
		}
		defer gz.Close()
		input = gz
	}
	tr := tar.NewReader(io.LimitReader(input, maxOuterExpanded+1))
	seen := make(map[string]byte)
	var expanded int64
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return idx, err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return idx, fmt.Errorf("read TAR: %w", err)
		}
		entries++
		if entries > maxOuterEntries {
			return idx, fmt.Errorf("TAR entry limit exceeded")
		}
		name, dir, err := cleanArchiveName(h.Name)
		if err != nil {
			return idx, err
		}
		kind := byte('f')
		if h.Typeflag == tar.TypeDir {
			kind = 'd'
		} else if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return idx, fmt.Errorf("TAR links/special members are forbidden")
		}
		if dir {
			kind = 'd'
		}
		key := strings.ToLower(name)
		if prior, ok := seen[key]; ok {
			return idx, fmt.Errorf("duplicate TAR destination %q (types %c/%c)", name, prior, kind)
		}
		seen[key] = kind
		if kind == 'd' {
			idx.dirs = append(idx.dirs, name)
			continue
		}
		if h.Size < 0 || h.Size > maxOuterExpanded {
			return idx, fmt.Errorf("TAR member exceeds expanded limit")
		}
		expanded += h.Size
		if expanded > maxOuterExpanded {
			return idx, fmt.Errorf("TAR expanded limit exceeded")
		}
		keep := selected == nil || selected[name]
		if keep {
			b, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
			if err != nil {
				return idx, err
			}
			if int64(len(b)) != h.Size {
				return idx, fmt.Errorf("TAR member size mismatch")
			}
			idx.files = append(idx.files, tarFile{name: name, data: b})
		} else if _, err := io.CopyN(io.Discard, tr, h.Size); err != nil {
			return idx, err
		}
	}
	if expanded > maxOuterExpanded {
		return idx, fmt.Errorf("TAR expanded limit exceeded")
	}
	return idx, nil
}

func cleanArchiveName(raw string) (string, bool, error) {
	if !utf8.ValidString(raw) || strings.Contains(raw, `\`) || strings.HasPrefix(raw, "/") || filepath.VolumeName(raw) != "" {
		return "", false, fmt.Errorf("unsafe archive path")
	}
	dir := strings.HasSuffix(raw, "/")
	raw = strings.TrimSuffix(raw, "/")
	raw = strings.TrimPrefix(raw, "./")
	if raw == "" {
		return "", dir, nil
	}
	p := path.Clean(raw)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") || p != raw {
		return "", false, fmt.Errorf("unsafe archive path %q", raw)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false, fmt.Errorf("unsafe archive path %q", raw)
		}
	}
	return p, dir, nil
}

func chooseSnapshot(idx tarIndex) (string, []string, error) {
	mods := make([]string, 0, 4)
	files := make(map[string]struct{}, len(idx.files))
	for _, f := range idx.files {
		files[f.name] = struct{}{}
	}
	for name := range files {
		if path.Base(name) == "go.mod" {
			mods = append(mods, path.Dir(name))
		}
	}
	if len(mods) == 0 {
		var archives []string
		for _, f := range idx.files {
			if isSupportedArchive(f.name) {
				archives = append(archives, f.name)
			}
		}
		sort.Strings(archives)
		if len(archives) != 1 {
			return "", nil, fmt.Errorf("TAR contains no unique Go source snapshot")
		}
		return archives[0], nil, nil
	}
	sort.Slice(mods, func(i, j int) bool {
		if strings.Count(mods[i], "/") != strings.Count(mods[j], "/") {
			return strings.Count(mods[i], "/") < strings.Count(mods[j], "/")
		}
		return mods[i] < mods[j]
	})
	minDepth := strings.Count(mods[0], "/")
	var roots []string
	for _, m := range mods {
		if strings.Count(m, "/") == minDepth {
			roots = append(roots, m)
		}
	}
	roots = uniqueStrings(roots)
	if len(roots) != 1 {
		return "", nil, fmt.Errorf("TAR has multiple equally shallow Go module roots")
	}
	root := roots[0]
	if root == "." {
		root = ""
	}
	var promptMembers []string
	for name := range files {
		rel := name
		if root != "" {
			if !strings.HasPrefix(name, root+"/") {
				continue
			}
			rel = strings.TrimPrefix(name, root+"/")
		}
		base := strings.ToLower(path.Base(rel))
		if base == "instruction.md" && path.Dir(rel) == "." {
			promptMembers = append(promptMembers, name)
		}
	}
	sort.Strings(promptMembers)
	return root, promptMembers, nil
}

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func snapshotWithinLimits(idx tarIndex, member string) bool {
	files, total := 0, 0
	for _, f := range idx.files {
		if f.name == member && isSupportedArchive(f.name) {
			return len(f.data) > 0 && len(f.data) <= maxOuterCompressed
		}
		if member != "" && !strings.HasPrefix(f.name, member+"/") {
			continue
		}
		if ignoredImportPath(f.name) {
			continue
		}
		files++
		if len(f.data) > task.MaxFileBytes {
			return false
		}
		total += len(f.data)
		if total > task.MaxBundleBytes || files > 1000 {
			return false
		}
	}
	return files > 0
}

func snapshotDigest(idx tarIndex, member string) string {
	for _, f := range idx.files {
		if member != "" && f.name == member {
			sum := sha256.Sum256(f.data)
			return hex.EncodeToString(sum[:])
		}
	}
	h := sha256.New()
	files := append([]tarFile(nil), idx.files...)
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	var total int64
	for _, f := range files {
		name := f.name
		if member != "" {
			if !strings.HasPrefix(name, member+"/") {
				continue
			}
			name = strings.TrimPrefix(name, member+"/")
		}
		if ignoredImportPath(name) {
			continue
		}
		var size [8]byte
		for i := 7; i >= 0; i-- {
			size[i] = byte(uint64(len(f.data)) >> (8 * (7 - i)))
		}
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(size[:])
		h.Write(f.data)
		total += int64(len(f.data))
	}
	_ = total
	return hex.EncodeToString(h.Sum(nil))
}

func ignoredImportPath(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") || strings.EqualFold(part, ".cache") || strings.EqualFold(part, "__MACOSX") || strings.HasPrefix(part, "._") {
			return true
		}
	}
	return false
}
