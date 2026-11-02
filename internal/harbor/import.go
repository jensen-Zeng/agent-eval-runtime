package harbor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
)

type inspectResult struct {
	Compatible             bool     `json:"compatible"`
	Version                string   `json:"version,omitempty"`
	TaskID                 string   `json:"task_id,omitempty"`
	Task                   string   `json:"task,omitempty"`
	AgentTimeoutSeconds    int      `json:"agent_timeout_seconds,omitempty"`
	VerifierTimeoutSeconds int      `json:"verifier_timeout_seconds,omitempty"`
	RequestedMemoryMiB     int64    `json:"requested_memory_mib,omitempty"`
	StorageRequestMiB      int64    `json:"storage_requested_but_not_enforced_mib,omitempty"`
	RequiredOutputs        []string `json:"required_outputs,omitempty"`
	Files                  int      `json:"files,omitempty"`
	Bytes                  int      `json:"bytes,omitempty"`
	Reason                 string   `json:"reason,omitempty"`
	Warnings               []string `json:"review_warnings,omitempty"`
}

func Inspect(path string) inspectResult {
	s, files, err := ValidatePackage(path)
	if err != nil {
		return inspectResult{Compatible: false, Reason: err.Error()}
	}
	var total int
	for _, b := range files {
		total += len(b)
	}
	id := s.Metadata.InstanceID
	if id == "" {
		id = filepath.Base(filepath.Clean(path))
	}
	if !taskIDPattern.MatchString(id) {
		return inspectResult{Compatible: false, Version: s.Version, TaskID: id, Reason: "metadata.instance_id must be a safe task ID"}
	}
	return inspectResult{Compatible: true, Version: s.Version, TaskID: id, Task: s.Metadata.Task, AgentTimeoutSeconds: int(s.Agent.TimeoutSec), VerifierTimeoutSeconds: int(s.Verifier.TimeoutSec), RequestedMemoryMiB: s.Environment.MemoryMB, StorageRequestMiB: s.Environment.StorageMB, RequiredOutputs: s.Other.RequiredOutputs, Files: len(files), Bytes: total, Warnings: CredentialWarnings(files)}
}

var credentialPattern = regexp.MustCompile(`(?i)(api[_-]?key|password|secret|credential|private[_-]?key|access[_-]?token)\s*[:=]\s*[^\s"']+`)
var rawCredentialPattern = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{10,}|AKIA[0-9A-Z]{16}|Bearer\s+[A-Za-z0-9._~+/-]{8,})\b`)

// CredentialWarnings provides review hints only; arbitrary task text is not
// rejected based on a credential-shaped substring.
func CredentialWarnings(files map[string][]byte) []string {
	var warnings []string
	for name, data := range files {
		if credentialPattern.Match(data) || rawCredentialPattern.Match(data) {
			warnings = append(warnings, "review possible credential-like content in "+name)
		}
	}
	sort.Strings(warnings)
	return warnings
}

type ImportOptions struct {
	Path, Project, ID string
	HelperImageID     string
	DockerHost        string
	MemoryMiB         int64
	VerifierTmpfs     []string
	Stdout, Stderr    io.Writer
}

func Import(ctx context.Context, o ImportOptions) (domain.Task, error) {
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	spec, allFiles, err := ValidatePackage(o.Path)
	if err != nil {
		return domain.Task{}, err
	}
	if o.ID == "" {
		o.ID = spec.Metadata.InstanceID
	}
	if !taskIDPattern.MatchString(o.ID) {
		return domain.Task{}, errors.New("--id or metadata.instance_id must be a safe task ID")
	}
	if o.Project == "" {
		o.Project = "."
	}
	project, err := filepath.Abs(o.Project)
	if err != nil {
		return domain.Task{}, err
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return domain.Task{}, err
	}
	if o.MemoryMiB != 0 && (o.MemoryMiB < 1024 || o.MemoryMiB > 16384) {
		return domain.Task{}, errors.New("--memory-mib must be between 1024 and 16384")
	}
	if err := sandbox.ValidateWritableDirs(o.VerifierTmpfs); err != nil {
		return domain.Task{}, fmt.Errorf("invalid verifier tmpfs override: %w", err)
	}
	root, err := os.MkdirTemp("", "agenteval-harbor-")
	if err != nil {
		return domain.Task{}, err
	}
	defer os.RemoveAll(root)
	packageRoot := filepath.Join(root, "package")
	selected := selectedPackage(allFiles)
	for _, warning := range CredentialWarnings(selected) {
		fmt.Fprintln(o.Stderr, "review:", warning)
	}
	if err := writePackageFiles(o.Path, packageRoot, selected); err != nil {
		return domain.Task{}, err
	}
	packageHash := HashFiles(selected)
	// The CLI ensures the runtime helper image first. Give that immutable ID a
	// local alias usable in a Dockerfile FROM instruction.
	helperID := o.HelperImageID
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(helperID) {
		return domain.Task{}, errors.New("a verified runtime helper image ID is required")
	}
	helperAlias := "agenteval-harbor-helper:" + shortHash(helperID)
	if err := runDocker(ctx, o.Stderr, o.DockerHost, "image", "tag", helperID, helperAlias); err != nil {
		return domain.Task{}, err
	}
	imagesRoot := filepath.Join(root, "images")
	if err := os.MkdirAll(imagesRoot, 0700); err != nil {
		return domain.Task{}, err
	}
	baseAgentTag := "agenteval-harbor-base-agent:" + shortHash(packageHash)
	baseVerifierTag := "agenteval-harbor-base-verifier:" + shortHash(packageHash)
	if err := dockerBuild(ctx, o.Stderr, o.DockerHost, filepath.Join(packageRoot, "environment"), baseAgentTag); err != nil {
		return domain.Task{}, fmt.Errorf("build Harbor agent image: %w", err)
	}
	if err := dockerBuild(ctx, o.Stderr, o.DockerHost, filepath.Join(packageRoot, "tests"), baseVerifierTag); err != nil {
		return domain.Task{}, fmt.Errorf("build Harbor verifier image: %w", err)
	}
	agentBaseID, err := imageID(ctx, o.DockerHost, baseAgentTag)
	if err != nil {
		return domain.Task{}, err
	}
	verifierBaseID, err := imageID(ctx, o.DockerHost, baseVerifierTag)
	if err != nil {
		return domain.Task{}, err
	}
	if err = stableImageID(ctx, o.DockerHost, baseAgentTag, agentBaseID); err != nil {
		return domain.Task{}, err
	}
	if err = stableImageID(ctx, o.DockerHost, baseVerifierTag, verifierBaseID); err != nil {
		return domain.Task{}, err
	}
	if err = writeWrapper(ctx, o.DockerHost, filepath.Join(imagesRoot, "agent"), agentBaseID, helperAlias, false); err != nil {
		return domain.Task{}, err
	}
	if err = writeWrapper(ctx, o.DockerHost, filepath.Join(imagesRoot, "verifier"), verifierBaseID, helperAlias, true); err != nil {
		return domain.Task{}, err
	}
	agentTag := "agenteval-harbor-agent:" + shortHash(packageHash)
	verifierTag := "agenteval-harbor-verifier:" + shortHash(packageHash)
	if err := dockerBuild(ctx, o.Stderr, o.DockerHost, filepath.Join(imagesRoot, "agent"), agentTag); err != nil {
		return domain.Task{}, fmt.Errorf("build Harbor agent runtime image: %w", err)
	}
	if err := dockerBuild(ctx, o.Stderr, o.DockerHost, filepath.Join(imagesRoot, "verifier"), verifierTag); err != nil {
		return domain.Task{}, fmt.Errorf("build Harbor verifier runtime image: %w", err)
	}
	agentID, err := imageID(ctx, o.DockerHost, agentTag)
	if err != nil {
		return domain.Task{}, err
	}
	verifierID, err := imageID(ctx, o.DockerHost, verifierTag)
	if err != nil {
		return domain.Task{}, err
	}
	if err = stableImageID(ctx, o.DockerHost, agentTag, agentID); err != nil {
		return domain.Task{}, err
	}
	if err = stableImageID(ctx, o.DockerHost, verifierTag, verifierID); err != nil {
		return domain.Task{}, err
	}
	task, err := newTask(o.ID, "package", packageHash, spec, selected, o.MemoryMiB, agentID, verifierID)
	if err != nil {
		return domain.Task{}, err
	}
	task.Harbor.VerifierTmpfs = append([]string(nil), o.VerifierTmpfs...)
	for _, dir := range []string{filepath.Join(project, "tasks"), filepath.Join(project, "tasks", "harbor")} {
		if err = ensureRealDirWithin(project, dir); err != nil {
			return domain.Task{}, err
		}
	}
	destination := filepath.Join(project, "tasks", "harbor", o.ID)
	parent := filepath.Dir(destination)
	if _, err = os.Lstat(destination); err == nil {
		return domain.Task{}, errors.New("Harbor task destination already exists; refusing to overwrite")
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.Task{}, err
	}
	stage, err := os.MkdirTemp(parent, ".harbor-"+o.ID+"-")
	if err != nil {
		return domain.Task{}, err
	}
	defer os.RemoveAll(stage)
	if err = os.Mkdir(filepath.Join(stage, "package"), 0700); err != nil {
		return domain.Task{}, err
	}
	if err = writePackageFiles(packageRoot, filepath.Join(stage, "package"), selected); err != nil {
		return domain.Task{}, err
	}
	if err = writeJSON(filepath.Join(stage, "task.json"), task); err != nil {
		return domain.Task{}, err
	}
	if _, err = os.Lstat(destination); err == nil {
		return domain.Task{}, errors.New("Harbor task destination appeared during import")
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.Task{}, err
	}
	if err = os.Rename(stage, destination); err != nil {
		return domain.Task{}, fmt.Errorf("publish Harbor task atomically: %w", err)
	}
	fmt.Fprintf(o.Stdout, "Imported Harbor task %s\nagent image: %s\nverifier image: %s\npackage sha256: %s\nsource sha256: %s\ntests sha256: %s\nstorage request: %d MiB (not enforced as a disk quota)\n", o.ID, agentID, verifierID, packageHash, task.SourceSHA256, task.EvaluatorSHA256, spec.Environment.StorageMB)
	return task, nil
}

func selectedPackage(files map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for name, data := range files {
		if name == "task.toml" || name == "instruction.md" || strings.HasPrefix(name, "environment/") || strings.HasPrefix(name, "tests/") || strings.HasPrefix(name, "solution/") {
			out[name] = data
		}
	}
	return out
}

func writeFiles(root string, files map[string][]byte) error {
	for name, data := range files {
		if !safeRel(name) {
			return fmt.Errorf("unsafe package path %q", name)
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			return err
		}
	}
	return nil
}

func writePackageFiles(sourceRoot, dest string, files map[string][]byte) error {
	for name, expected := range files {
		if !safeRel(name) {
			return fmt.Errorf("unsafe package path %q", name)
		}
		source := filepath.Join(sourceRoot, filepath.FromSlash(name))
		info, err := os.Lstat(source)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("package file changed or became unsafe: %s", name)
		}
		actual, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if !bytes.Equal(actual, expected) {
			return fmt.Errorf("package file changed during import: %s", name)
		}
		mode := info.Mode().Perm() & 0755
		if mode&0400 == 0 {
			mode |= 0444
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err = os.WriteFile(target, actual, mode); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0600)
}

func ensureRealDirWithin(root, dir string) error {
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("task import parent must be a real directory: %s", dir)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("task import directory escapes project root")
	}
	return nil
}
func shortHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:8])
}

func dockerBuild(ctx context.Context, log io.Writer, host, contextDir, tag string) error {
	return runDocker(ctx, log, host, "build", "--tag", tag, contextDir)
}
func runDocker(ctx context.Context, log io.Writer, host string, args ...string) error {
	cmd := dockerCommand(ctx, host, args...)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
func dockerCommand(ctx context.Context, host string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if strings.TrimSpace(host) != "" {
		for _, item := range os.Environ() {
			key := item
			if i := strings.IndexByte(item, '='); i >= 0 {
				key = item[:i]
			}
			if strings.EqualFold(key, "DOCKER_HOST") || strings.EqualFold(key, "DOCKER_CONTEXT") {
				continue
			}
			cmd.Env = append(cmd.Env, item)
		}
		cmd.Env = append(cmd.Env, "DOCKER_HOST="+host)
	}
	return cmd
}
func imageID(ctx context.Context, host, ref string) (string, error) {
	cmd := dockerCommand(ctx, host, "image", "inspect", "--format", "{{.Id}}", ref)
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("inspect built image %s: %w", ref, err)
	}
	id := strings.TrimSpace(string(b))
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(id) {
		return "", fmt.Errorf("Docker returned invalid image ID for %s", ref)
	}
	return id, nil
}
func stableImageID(ctx context.Context, host, ref, expected string) error {
	current, err := imageID(ctx, host, ref)
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("image reference %s moved during import", ref)
	}
	return nil
}
func writeWrapper(ctx context.Context, host, dir, baseID, helperAlias string, verifier bool) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	baseAlias := "agenteval-harbor-pinned:" + shortHash(baseID)
	if err := runDocker(ctx, io.Discard, host, "image", "tag", baseID, baseAlias); err != nil {
		return err
	}
	commands := "mkdir -p /opt/agenteval-seed /app && cp -a /app/. /opt/agenteval-seed/ && chown -R 10001:10001 /opt/agenteval-seed /app"
	if verifier {
		commands += " && chmod -R a+rX /tests"
	}
	dockerfile := fmt.Sprintf("FROM %s\nCOPY --from=%s /usr/local/bin/sandbox-helper /usr/local/bin/sandbox-helper\nUSER 0\nRUN %s\nUSER 10001:10001\nWORKDIR /app\nENTRYPOINT [\"/bin/sleep\", \"infinity\"]\n", baseAlias, helperAlias, commands)
	return os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0600)
}
