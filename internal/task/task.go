// Package task loads trusted local task manifests and verifies immutable inputs.
package task

import (
	"crypto/sha256"
	"encoding/binary"
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

	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
)

const MaxFileBytes = 1 << 20
const MaxBundleBytes = 50 << 20

var taskID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)

func ValidPath(p string) bool {
	return p != "" && p != "." && !strings.ContainsAny(p, "\\:\x00\r\n\t") && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && !strings.HasPrefix(p, "-")
}

// HashFiles includes paths and sizes, so neither renaming nor boundary ambiguity is ignored.
func HashFiles(files sandbox.Files) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	var size [8]byte
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0})
		binary.BigEndian.PutUint64(size[:], uint64(len(files[n])))
		h.Write(size[:])
		h.Write(files[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ReadBundle(dir string) (sandbox.Files, error) {
	result := sandbox.Files{}
	total := 0
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("bundle must be a real directory")
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == dir {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("links forbidden: %s", d.Name())
		}
		if d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special files forbidden")
		}
		rel, e := filepath.Rel(dir, p)
		if e != nil {
			return e
		}
		name := filepath.ToSlash(rel)
		for _, part := range strings.Split(name, "/") {
			if strings.EqualFold(part, ".git") {
				return fmt.Errorf("Git metadata is forbidden in task bundles")
			}
		}
		if !ValidPath(name) {
			return fmt.Errorf("invalid bundle path %q", name)
		}
		if info.Size() > MaxFileBytes || len(result) >= 1000 {
			return fmt.Errorf("bundle file limit exceeded")
		}
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		data, e := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
		closeErr := f.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil {
			return e
		}
		total += len(data)
		if len(data) > MaxFileBytes || total > MaxBundleBytes {
			return fmt.Errorf("bundle size exceeded")
		}
		result[name] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("empty bundle")
	}
	return result, nil
}

func DecodeStrict(r io.Reader, out any) error {
	d := json.NewDecoder(io.LimitReader(r, 2<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected a single JSON object")
	}
	return nil
}

func Load(manifest, projectRoot string) (domain.Task, error) {
	t := domain.Task{Budget: domain.DefaultBudget(), Limits: sandbox.Limits{CPUs: 1, MemoryMiB: 1024, Pids: 64, WorkspaceMiB: 64}}
	f, err := os.Open(manifest)
	if err != nil {
		return t, err
	}
	defer f.Close()
	if err = DecodeStrict(f, &t); err != nil {
		return t, err
	}
	if t.SchemaVersion != 1 || !taskID.MatchString(t.ID) || t.Revision < 1 || strings.TrimSpace(t.Prompt) == "" || len(t.Prompt) > 32768 {
		return t, fmt.Errorf("invalid task identity/schema/prompt")
	}
	if len(t.Metadata) > 32 {
		return t, fmt.Errorf("too many metadata fields")
	}
	for k, v := range t.Metadata {
		if len(k) == 0 || len(k) > 80 || len(v) > 2048 || strings.ContainsAny(k+v, "\x00\r\n") {
			return t, fmt.Errorf("invalid task metadata")
		}
	}
	if p := t.Environment.GoModuleCache; p != "" {
		if !strings.HasPrefix(p, "/opt/") || !ValidPath(strings.TrimPrefix(p, "/")) {
			return t, fmt.Errorf("module cache must be an image-owned path under /opt")
		}
	}
	if err = ValidateBudget(t.Budget); err != nil {
		return t, err
	}
	if t.Limits.TmpMiB != 0 && (t.Limits.TmpMiB < 64 || t.Limits.TmpMiB > 1024 || t.Limits.TmpMiB > t.Limits.MemoryMiB) {
		return t, fmt.Errorf("tmp_mib must be 64..1024 and no greater than memory_mib (0 keeps 256 MiB default)")
	}
	if t.Limits.CPUs < 0.1 || t.Limits.CPUs > 4 || t.Limits.MemoryMiB < 128 || t.Limits.MemoryMiB > 2048 || t.Limits.Pids < 16 || t.Limits.Pids > 256 || t.Limits.WorkspaceMiB < 8 || t.Limits.WorkspaceMiB > 128 {
		return t, fmt.Errorf("requested resource limits exceed local policy")
	}
	if len(t.AllowedFiles) == 0 || (t.Grader == nil && len(t.RequiredTests) == 0) || len(t.AllowedFiles) > 100 || len(t.RequiredTests) > 100 {
		return t, fmt.Errorf("allowed_files and required_tests are required")
	}
	seen := map[string]bool{}
	for _, n := range t.AllowedFiles {
		if !ValidPath(n) || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || seen[n] {
			return t, fmt.Errorf("allowed source path must be a unique non-test Go file: %q", n)
		}
		seen[n] = true
	}
	seen = map[string]bool{}
	testName := regexp.MustCompile(`^Test[A-Za-z0-9_]+$`)
	for _, n := range t.RequiredTests {
		if !testName.MatchString(n) || seen[n] {
			return t, fmt.Errorf("invalid/duplicate required test %q", n)
		}
		seen[n] = true
	}
	base, _ := filepath.Abs(filepath.Dir(manifest))
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return t, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return t, err
	}
	resolve := func(p string) (string, error) {
		if p == "" || filepath.IsAbs(p) {
			return "", fmt.Errorf("bundle path must be relative to manifest")
		}
		full, err := filepath.EvalSymlinks(filepath.Join(base, p))
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(root, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("bundle outside project root")
		}
		return full, nil
	}
	source, err := resolve(t.SourceDir)
	if err != nil {
		return t, err
	}
	eval, err := resolve(t.EvaluatorDir)
	if err != nil {
		return t, err
	}
	if t.Source, err = ReadBundle(source); err != nil {
		return t, err
	}
	if t.Evaluator, err = ReadBundle(eval); err != nil {
		return t, err
	}
	if HashFiles(t.Source) != t.SourceSHA256 {
		return t, fmt.Errorf("source SHA256 mismatch; update task only after reviewing source")
	}
	if HashFiles(t.Evaluator) != t.EvaluatorSHA256 {
		return t, fmt.Errorf("evaluator SHA256 mismatch")
	}
	if _, ok := t.Source["go.mod"]; !ok {
		return t, fmt.Errorf("source go.mod required")
	}
	if t.Grader != nil {
		if t.Grader.Kind != "python" || !ValidPath(t.Grader.EntryPoint) || !strings.HasSuffix(t.Grader.EntryPoint, ".py") {
			return t, fmt.Errorf("invalid trusted grader")
		}
		if _, ok := t.Evaluator[t.Grader.EntryPoint]; !ok {
			return t, fmt.Errorf("grader entrypoint missing")
		}
		for n := range t.Source {
			if strings.HasPrefix(n, ".agenteval-evaluator/") || n == ".agenteval-evaluator" {
				return t, fmt.Errorf("reserved grader source path")
			}
		}
	}
	for n := range t.Evaluator {
		if t.Grader == nil && !strings.HasSuffix(n, "_test.go") {
			return t, fmt.Errorf("evaluator bundle may contain only trusted *_test.go files")
		}
	}
	return t, nil
}

func ValidateBudget(b domain.Budget) error {
	values := []int{b.RunSeconds, b.PrepareSeconds, b.CommandSeconds, b.ModelSeconds, b.CollectSeconds, b.EvalSeconds, b.CleanupSeconds, b.MaxSteps, b.MaxTokens, b.MaxOutputTokens}
	for _, v := range values {
		if v <= 0 {
			return fmt.Errorf("all budget values must be positive")
		}
	}
	if b.RunSeconds > 1800 || b.PrepareSeconds > 300 || b.CommandSeconds > 120 || b.ModelSeconds > 120 || b.CollectSeconds > 120 || b.EvalSeconds > 300 || b.CleanupSeconds > 60 || b.MaxSteps > 100 || b.MaxTokens > 100000 || b.MaxOutputTokens > b.MaxTokens {
		return fmt.Errorf("budget exceeds local policy")
	}
	if b.RunSeconds <= b.PrepareSeconds+b.CollectSeconds+b.EvalSeconds+15 {
		return fmt.Errorf("run budget must leave Agent time after prepare/collect/eval/finalize reserves")
	}
	return nil
}
