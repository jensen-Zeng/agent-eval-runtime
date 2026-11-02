package harbor

// Package harbor validates and imports the supported T3-RL Harbor task subset.
// It deliberately keeps task-provided code in separate agent and verifier images.
import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"github.com/pelletier/go-toml/v2"
)

const (
	Version            = "T3-RL-v1.0"
	MaxPackageBytes    = 128 << 20
	MaxPackageFiles    = 10000
	MaxBundleBytes     = 64 << 20
	MaxBundleFileBytes = 16 << 20
)

var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)
var forbiddenCredential = regexp.MustCompile(`(?i)(api[_-]?key|password|secret|credential|private[_-]?key|access[_-]?token)\s*[:=]\s*[^\s"']+`)
var rawToken = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{10,}|AKIA[0-9A-Z]{16}|Bearer\s+[A-Za-z0-9._~+/-]{8,})\b`)

type taskTOML struct {
	Version   string   `toml:"version"`
	Artifacts []string `toml:"artifacts"`
	Topic     struct {
		L1 string `toml:"L1"`
		L2 string `toml:"L2"`
		L3 string `toml:"L3"`
	} `toml:"topic"`
	Metadata struct {
		AuthorName         string `toml:"author_name"`
		InstanceID         string `toml:"instance_id"`
		Task               string `toml:"task"`
		Source             string `toml:"source"`
		Language           string `toml:"language"`
		PromptCreationTime string `toml:"prompt_creation_time"`
	} `toml:"metadata"`
	Environment struct {
		DockerImage string  `toml:"docker_image"`
		CPUs        float64 `toml:"cpus"`
		MemoryMB    int64   `toml:"memory_mb"`
		StorageMB   int64   `toml:"storage_mb"`
		Workdir     string  `toml:"workdir"`
		NetworkMode string  `toml:"network_mode"`
		SkipUpload  bool    `toml:"skip_upload"`
		Upload      struct {
			Source string `toml:"source"`
			Target string `toml:"target"`
		} `toml:"upload"`
		Env map[string]string `toml:"env"`
	} `toml:"environment"`
	Agent struct {
		TimeoutSec float64 `toml:"timeout_sec"`
	} `toml:"agent"`
	Verifier struct {
		EnvironmentMode string            `toml:"environment_mode"`
		TimeoutSec      float64           `toml:"timeout_sec"`
		Env             map[string]string `toml:"env"`
	} `toml:"verifier"`
	Judge struct {
		Type       string `toml:"type"`
		RewardKind string `toml:"reward_kind"`
	} `toml:"judge"`
	Other struct {
		Grader          string   `toml:"grader"`
		GradingStatus   string   `toml:"grading_status"`
		RequiredOutputs []string `toml:"required_outputs"`
	} `toml:"other"`
	RolloutBaseline map[string]struct {
		Reward float64 `toml:"reward"`
	} `toml:"rollout_baseline"`
}

// ReadFiles reads a real directory tree without following links or including
// platform metadata. Returned names use slash separators relative to root.
func ReadFiles(root string, maxFile, maxTotal int64, maxFiles int) (map[string][]byte, error) {
	return readFiles(root, maxFile, maxTotal, maxFiles, nil)
}

func readPackageFiles(root string) (map[string][]byte, error) {
	return readFiles(root, MaxPackageBytes, MaxPackageBytes, MaxPackageFiles, func(rel string, isDir bool) bool {
		first := strings.SplitN(rel, "/", 2)[0]
		if isDir {
			return first == "environment" || first == "tests" || first == "solution"
		}
		return rel == "task.toml" || rel == "instruction.md" || strings.HasPrefix(rel, "environment/") || strings.HasPrefix(rel, "tests/") || strings.HasPrefix(rel, "solution/")
	})
}

func readFiles(root string, maxFile, maxTotal int64, maxFiles int, include func(string, bool) bool) (map[string][]byte, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Harbor input must be a real directory")
	}
	out := map[string][]byte{}
	var total int64
	err = filepath.WalkDir(root, func(name string, ent fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == root {
			return nil
		}
		rel, e := filepath.Rel(root, name)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if include != nil && !include(rel, ent.IsDir()) {
			if ent.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		base := filepath.Base(name)
		if ent.IsDir() && (base == "node_modules" || base == ".venv" || base == "__pycache__" || base == ".pytest_cache") {
			return filepath.SkipDir
		}
		if base == "__MACOSX" || strings.HasPrefix(base, "._") || base == ".DS_Store" {
			if ent.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(base, ".git") || strings.EqualFold(base, ".env") {
			return fmt.Errorf("Git metadata or environment credential files are forbidden: %s", rel)
		}
		if ent.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("links are forbidden in Harbor package: %s", rel)
		}
		if ent.IsDir() {
			if strings.EqualFold(base, ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		info, e := ent.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special file is forbidden: %s", rel)
		}
		if strings.Contains(strings.ToLower("/"+rel+"/"), "/.git/") {
			return fmt.Errorf("Git metadata is forbidden: %s", rel)
		}
		if info.Size() < 0 || info.Size() > maxFile {
			return fmt.Errorf("file exceeds limit: %s", rel)
		}
		if len(out) >= maxFiles {
			return errors.New("Harbor package file count exceeds limit")
		}
		f, e := os.Open(name)
		if e != nil {
			return e
		}
		data, e := io.ReadAll(io.LimitReader(f, maxFile+1))
		closeErr := f.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil {
			return e
		}
		if int64(len(data)) > maxFile {
			return fmt.Errorf("file grew beyond limit while reading: %s", rel)
		}
		total += int64(len(data))
		if total > maxTotal {
			return errors.New("Harbor package exceeds size limit")
		}
		out[rel] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("Harbor package is empty")
	}
	return out, nil
}

// HashFiles uses the same canonical path/length/content digest as task.HashFiles.
func HashFiles(files map[string][]byte) string {
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

func readManifest(manifest string) (taskTOML, []byte, error) {
	var spec taskTOML
	b, err := os.ReadFile(manifest)
	if err != nil {
		return spec, nil, err
	}
	if len(b) > 1<<20 {
		return spec, nil, errors.New("task.toml exceeds 1 MiB")
	}
	d := toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields()
	if err = d.Decode(&spec); err != nil {
		return spec, nil, fmt.Errorf("parse Harbor task.toml: %w", err)
	}
	return spec, b, nil
}

func ValidatePackage(root string) (taskTOML, map[string][]byte, error) {
	var zero taskTOML
	files, err := readPackageFiles(root)
	if err != nil {
		return zero, nil, err
	}
	manifestBytes, ok := files["task.toml"]
	if !ok {
		return zero, nil, errors.New("task.toml is required")
	}
	var spec taskTOML
	if err = toml.NewDecoder(bytes.NewReader(manifestBytes)).DisallowUnknownFields().Decode(&spec); err != nil {
		return zero, nil, fmt.Errorf("parse Harbor task.toml: %w", err)
	}
	if err = validateSpec(spec, files); err != nil {
		return zero, nil, err
	}
	return spec, files, nil
}

func validateSpec(s taskTOML, files map[string][]byte) error {
	if s.Version != Version {
		return fmt.Errorf("unsupported Harbor version %q", s.Version)
	}
	if s.Metadata.InstanceID == "" || !taskIDPattern.MatchString(s.Metadata.InstanceID) || strings.TrimSpace(s.Metadata.Task) == "" {
		return errors.New("metadata.instance_id and metadata.task are required")
	}
	if !containsString(s.Artifacts, "/app") {
		return errors.New("artifacts must include /app")
	}
	if s.Environment.DockerImage != "" {
		return errors.New("prebuilt-only Harbor tasks are unsupported; provide environment/Dockerfile and tests/Dockerfile")
	}
	if s.Environment.NetworkMode != "no-network" {
		return errors.New("Harbor network_mode must be no-network")
	}
	if s.Environment.Workdir != "/app" || s.Environment.Upload.Target != "/app" || s.Environment.Upload.Source != "environment/workspace" || s.Environment.SkipUpload {
		return errors.New("Harbor upload/workdir must map environment/workspace to /app")
	}
	if s.Verifier.EnvironmentMode != "separate" {
		return errors.New("only separate verifier environments are supported")
	}
	if len(s.Verifier.Env) != 0 {
		return errors.New("verifier environment variables are unsupported; verifier secrets are forbidden")
	}
	if s.Judge.Type != "programmatic" || s.Judge.RewardKind != "continuous" {
		return errors.New("only programmatic continuous reward is supported")
	}
	if s.Other.Grader == "" || !safeRel(s.Other.Grader) || !strings.HasPrefix(s.Other.Grader, "tests/") {
		return errors.New("other.grader must identify a tests/ entrypoint")
	}
	if _, ok := files[s.Other.Grader]; !ok {
		return fmt.Errorf("grader entrypoint missing: %s", s.Other.Grader)
	}
	if _, ok := files["instruction.md"]; !ok {
		return errors.New("instruction.md is required")
	}
	if !utf8.Valid(files["instruction.md"]) || bytes.IndexByte(files["instruction.md"], 0) >= 0 {
		return errors.New("instruction.md must be UTF-8 text without NUL bytes")
	}
	if _, ok := files["environment/Dockerfile"]; !ok {
		return errors.New("environment/Dockerfile is required")
	}
	if !regexp.MustCompile(`(?im)^\s*COPY\s+workspace/\s+/app/\s*$`).Match(files["environment/Dockerfile"]) {
		return errors.New("environment/Dockerfile must COPY workspace/ into /app/")
	}
	if _, ok := files["environment/workspace/.keep"]; !ok {
		found := false
		for n := range files {
			if strings.HasPrefix(n, "environment/workspace/") {
				found = true
				break
			}
		}
		if !found {
			return errors.New("environment/workspace must contain files")
		}
	}
	if _, ok := files["tests/Dockerfile"]; !ok {
		return errors.New("tests/Dockerfile is required")
	}
	if !regexp.MustCompile(`(?im)^\s*COPY\s+\.\s+/tests/?\s*$`).Match(files["tests/Dockerfile"]) {
		return errors.New("tests/Dockerfile must copy the verifier context into /tests")
	}
	if _, ok := files["tests/test.sh"]; !ok {
		return errors.New("tests/test.sh is required as the verifier adapter")
	}
	if !bytes.Contains(files["tests/test.sh"], []byte(filepath.Base(s.Other.Grader))) || !bytes.Contains(files["tests/test.sh"], []byte("/logs/verifier/reward.json")) {
		return errors.New("tests/test.sh must invoke the declared grader and write /logs/verifier/reward.json")
	}
	if len(s.Other.RequiredOutputs) == 0 {
		return errors.New("other.required_outputs is required")
	}
	for _, p := range s.Other.RequiredOutputs {
		if !safeRel(p) || strings.HasPrefix(p, "tests/") || strings.HasPrefix(p, "solution/") {
			return fmt.Errorf("invalid required output path %q", p)
		}
		if hasReservedOutputDir(p) {
			return fmt.Errorf("required output path %q uses a reserved metadata directory", p)
		}
	}
	if s.Environment.CPUs <= 0 || s.Environment.CPUs > 4 || s.Environment.MemoryMB < 128 || s.Environment.MemoryMB > 16384 || s.Environment.StorageMB < 1 {
		return errors.New("Harbor CPU, memory, or storage request exceeds supported bounds")
	}
	if s.Agent.TimeoutSec <= 0 || s.Agent.TimeoutSec > 86400 || s.Verifier.TimeoutSec <= 0 || s.Verifier.TimeoutSec > 3600 || s.Agent.TimeoutSec+s.Verifier.TimeoutSec+180 > 86400 {
		return errors.New("Harbor agent/verifier timeout exceeds supported bounds")
	}
	if s.Environment.CPUs > 4 {
		return errors.New("Harbor CPU request exceeds 4")
	}
	for k, v := range s.Environment.Env {
		lowerKey := strings.ToLower(k)
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(k) || strings.HasPrefix(lowerKey, "agenteval_") || strings.Contains(v, "${") || strings.ContainsAny(v, "\x00\r\n") || strings.Contains(lowerKey, "key") || strings.Contains(lowerKey, "token") || strings.Contains(lowerKey, "secret") || strings.Contains(lowerKey, "password") {
			return fmt.Errorf("unsupported or sensitive environment setting %q", k)
		}
		if rawToken.MatchString(v) || forbiddenCredential.MatchString(v) {
			return fmt.Errorf("credential-like value is forbidden in environment variable %q", k)
		}
	}
	return nil
}

func hasReservedOutputDir(p string) bool {
	for _, component := range strings.Split(p, "/") {
		switch strings.ToLower(component) {
		case ".git", "__pycache__", ".pytest_cache", "node_modules", ".venv":
			return true
		}
	}
	return false
}

func safeRel(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\:\x00\r\n") || filepath.IsAbs(p) || strings.HasPrefix(p, "-") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	return clean == p && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func subset(files map[string][]byte, prefix string, trim bool) map[string][]byte {
	out := map[string][]byte{}
	for n, b := range files {
		if strings.HasPrefix(n, prefix) {
			k := n
			if trim {
				k = strings.TrimPrefix(n, prefix)
			}
			if k != "" {
				out[k] = b
			}
		}
	}
	return out
}

// Load reads the frozen Harbor package copy named by Task.Harbor.PackageDir and
// reconstructs the runtime source/evaluator bundles after checking all hashes.
func Load(t domain.Task, manifest, projectRoot string) (domain.Task, error) {
	if t.Backend != "harbor" || t.Harbor == nil {
		return t, errors.New("Harbor backend spec is required")
	}
	if err := sandbox.ValidateWritableDirs(t.Harbor.VerifierTmpfs); err != nil {
		return t, fmt.Errorf("invalid Harbor verifier tmpfs override: %w", err)
	}
	if !taskIDPattern.MatchString(t.ID) || t.SchemaVersion != 1 || t.Revision < 1 || strings.TrimSpace(t.Prompt) == "" {
		return t, errors.New("invalid Harbor task identity or prompt")
	}
	if !safeRel(t.Harbor.PackageDir) {
		return t, errors.New("Harbor package_dir must be relative")
	}
	manifestAbs, err := filepath.Abs(manifest)
	if err != nil {
		return t, err
	}
	projectAbs, err := filepath.Abs(projectRoot)
	if err != nil {
		return t, err
	}
	base := filepath.Dir(manifestAbs)
	packageRoot := filepath.Join(base, filepath.FromSlash(t.Harbor.PackageDir))
	realPackage, err := filepath.EvalSymlinks(packageRoot)
	if err != nil {
		return t, err
	}
	realProject, err := filepath.EvalSymlinks(projectAbs)
	if err != nil {
		return t, err
	}
	rel, err := filepath.Rel(realProject, realPackage)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return t, errors.New("Harbor package is outside project root")
	}
	spec, files, err := ValidatePackage(realPackage)
	if err != nil {
		return t, err
	}
	if t.Harbor.PackageSHA256 == "" || HashFiles(files) != t.Harbor.PackageSHA256 {
		return t, errors.New("Harbor package SHA256 mismatch")
	}
	if spec.Environment.Workdir != t.Harbor.Workdir || t.Harbor.Workdir != "/app" || !imageIDPattern.MatchString(t.Harbor.AgentImage) || !imageIDPattern.MatchString(t.Harbor.VerifierImage) {
		return t, errors.New("Harbor workdir or frozen image IDs are invalid")
	}
	if t.Harbor.PassThreshold != 1 || !equalStrings(t.Harbor.RequiredOutputs, spec.Other.RequiredOutputs) || !equalStrings(t.Harbor.AgentEnv, envPairs(spec.Environment.Env)) || len(t.Harbor.VerifierEnv) != 0 {
		return t, errors.New("Harbor outputs, environment, or pass threshold differs from task.toml")
	}
	if t.Prompt != string(files["instruction.md"]) || t.Backend != "harbor" || t.Grader != nil || len(t.RequiredTests) != 0 || len(t.AllowedFiles) != 0 || t.Environment != (domain.ExecutionEnvironment{}) || t.SourceDir != "package/environment/workspace" || t.EvaluatorDir != "package/tests" || !noOpScripts(t.Scripts) {
		return t, errors.New("Harbor task manifest fields differ from the frozen package")
	}
	if t.Limits.CPUs != spec.Environment.CPUs || t.Limits.MemoryMiB < 1024 || t.Limits.MemoryMiB > 16384 || t.Limits.Pids != 256 || t.Limits.WorkspaceMiB != 1024 || t.Limits.TmpMiB != 1024 {
		return t, errors.New("Harbor resource limits exceed supported policy")
	}
	if t.Budget != budget(spec) {
		return t, errors.New("Harbor budget differs from task.toml")
	}
	source := subset(files, "environment/workspace/", true)
	evaluator := subset(files, "tests/", true)
	if len(source) == 0 || len(evaluator) == 0 || HashFiles(source) != t.SourceSHA256 || HashFiles(evaluator) != t.EvaluatorSHA256 {
		return t, errors.New("Harbor workspace or tests hash mismatch")
	}
	if err := checkBundleLimits(source); err != nil {
		return t, fmt.Errorf("Harbor workspace: %w", err)
	}
	if err := checkBundleLimits(evaluator); err != nil {
		return t, fmt.Errorf("Harbor tests: %w", err)
	}
	if !metadataMatches(t.Metadata, spec, t.Limits.MemoryMiB) {
		return t, errors.New("Harbor metadata differs from task.toml or effective limits")
	}
	for _, output := range t.Harbor.RequiredOutputs {
		if !safeRel(output) {
			return t, errors.New("invalid Harbor required output")
		}
	}
	t.Source, t.Evaluator = source, evaluator
	return t, nil
}

func checkBundleLimits(files map[string][]byte) error {
	if len(files) > 4000 {
		return errors.New("file count exceeds 4000")
	}
	total := 0
	for name, b := range files {
		if !safeRel(name) {
			return fmt.Errorf("unsafe path %q", name)
		}
		if len(b) > MaxBundleFileBytes {
			return fmt.Errorf("file %s exceeds 16 MiB", name)
		}
		total += len(b)
	}
	if total > MaxBundleBytes {
		return errors.New("bundle exceeds 64 MiB")
	}
	return nil
}

func metadataMatches(metadata map[string]string, spec taskTOML, effectiveMemory int64) bool {
	expected := map[string]string{"harbor_version": spec.Version, "language": spec.Metadata.Language, "task": spec.Metadata.Task, "requested_memory_mib": fmt.Sprint(spec.Environment.MemoryMB), "effective_memory_mib": fmt.Sprint(effectiveMemory), "requested_storage_mib_unenforced": fmt.Sprint(spec.Environment.StorageMB), "requested_cpus": fmt.Sprint(spec.Environment.CPUs), "agent_timeout_sec": fmt.Sprint(spec.Agent.TimeoutSec), "verifier_timeout_sec": fmt.Sprint(spec.Verifier.TimeoutSec)}
	if len(metadata) != len(expected) {
		return false
	}
	for key, value := range expected {
		if metadata[key] != value {
			return false
		}
	}
	return true
}

func noOpScripts(scripts map[string]domain.Script) bool {
	if len(scripts) != 2 {
		return false
	}
	for _, name := range []string{"baseline", "nop"} {
		script, ok := scripts[name]
		if !ok || len(script.Actions) != 1 || script.Actions[0].Tool != "finish" {
			return false
		}
		var args struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal(script.Actions[0].Arguments, &args) != nil || strings.TrimSpace(args.Summary) == "" {
			return false
		}
	}
	return true
}

var imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func budget(s taskTOML) domain.Budget {
	agentSec := int(s.Agent.TimeoutSec + 0.999999)
	verifySec := int(s.Verifier.TimeoutSec + 0.999999)
	if agentSec < 1 {
		agentSec = 1
	}
	if verifySec < 1 {
		verifySec = 1
	}
	cleanup, collect := 60, 120
	return domain.Budget{RunSeconds: agentSec + verifySec + cleanup + collect, PrepareSeconds: 300, CommandSeconds: 300, ModelSeconds: min(agentSec, 120), CollectSeconds: collect, EvalSeconds: verifySec, CleanupSeconds: cleanup, MaxSteps: 100, MaxTokens: 100000, MaxOutputTokens: 8192}
}

func newTask(id, packageDir, packageHash string, s taskTOML, files map[string][]byte, memoryMiB int64, agentImage, verifierImage string) (domain.Task, error) {
	if !taskIDPattern.MatchString(id) {
		return domain.Task{}, errors.New("invalid task ID")
	}
	if memoryMiB == 0 {
		memoryMiB = s.Environment.MemoryMB
	}
	if memoryMiB < 1024 || memoryMiB > 16384 {
		return domain.Task{}, errors.New("memory profile must be between 1024 and 16384 MiB")
	}
	source := subset(files, "environment/workspace/", true)
	evaluator := subset(files, "tests/", true)
	if len(source) == 0 || len(evaluator) == 0 {
		return domain.Task{}, errors.New("Harbor source/tests bundle is empty")
	}
	meta := map[string]string{"harbor_version": s.Version, "language": s.Metadata.Language, "task": s.Metadata.Task, "requested_memory_mib": fmt.Sprint(s.Environment.MemoryMB), "effective_memory_mib": fmt.Sprint(memoryMiB), "requested_storage_mib_unenforced": fmt.Sprint(s.Environment.StorageMB), "requested_cpus": fmt.Sprint(s.Environment.CPUs), "agent_timeout_sec": fmt.Sprint(s.Agent.TimeoutSec), "verifier_timeout_sec": fmt.Sprint(s.Verifier.TimeoutSec)}
	h := &domain.HarborSpec{PackageDir: packageDir, PackageSHA256: packageHash, AgentImage: agentImage, VerifierImage: verifierImage, Workdir: "/app", AgentEnv: envPairs(s.Environment.Env), RequiredOutputs: append([]string(nil), s.Other.RequiredOutputs...), PassThreshold: 1.0}
	return domain.Task{Backend: "harbor", Harbor: h, Metadata: meta, SchemaVersion: 1, ID: id, Revision: 1, Prompt: string(files["instruction.md"]), SourceDir: "package/environment/workspace", SourceSHA256: HashFiles(source), EvaluatorDir: "package/tests", EvaluatorSHA256: HashFiles(evaluator), Budget: budget(s), Limits: sandbox.Limits{CPUs: s.Environment.CPUs, MemoryMiB: memoryMiB, Pids: 256, WorkspaceMiB: 1024, TmpMiB: 1024}, Scripts: map[string]domain.Script{"baseline": {Actions: []domain.Action{{Tool: "finish", Arguments: []byte(`{"summary":"Harbor no-op baseline"}`)}}}, "nop": {Actions: []domain.Action{{Tool: "finish", Arguments: []byte(`{"summary":"Harbor no-op baseline"}`)}}}}}, nil
}

func envPairs(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func packageDigest(files map[string][]byte) string { return HashFiles(files) }
