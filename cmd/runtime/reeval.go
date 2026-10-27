package main

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/config"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/evaluator"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/patch"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/runner"
	dockersandbox "github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox/docker"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
)

const maxReevaluationPatchBytes = patch.MaxBytes

type reevaluationReport struct {
	SchemaVersion   int              `json:"schema_version"`
	RuntimeVersion  string           `json:"runtime_version"`
	ReevaluationID  string           `json:"reevaluation_id"`
	TaskID          string           `json:"task_id"`
	TaskRevision    int              `json:"task_revision"`
	ManifestSHA256  string           `json:"manifest_sha256"`
	SourceSHA256    string           `json:"source_sha256"`
	EvaluatorSHA256 string           `json:"evaluator_sha256"`
	ImageID         string           `json:"image_id"`
	PatchSHA256     string           `json:"patch_sha256"`
	PatchFile       string           `json:"patch_file"`
	StartedAt       time.Time        `json:"started_at"`
	FinishedAt      time.Time        `json:"finished_at"`
	WallMS          int64            `json:"wall_ms"`
	Verdict         string           `json:"verdict"`
	Classification  string           `json:"classification,omitempty"`
	Score           *float64         `json:"score,omitempty"`
	Evaluation      evaluator.Result `json:"evaluation"`
	CleanupStatus   string           `json:"cleanup_status"`
	ArtifactDir     string           `json:"artifact_dir"`
	Error           string           `json:"error,omitempty"`
}

func executeReevaluation(parent context.Context, args []string, out, errout io.Writer) int {
	fs := flag.NewFlagSet("runtime reeval", flag.ContinueOnError)
	fs.SetOutput(errout)
	configFile := fs.String("config", "", "local JSON configuration (optional)")
	project := fs.String("project", ".", "trusted project root")
	taskFile := fs.String("task", "", "trusted task manifest JSON")
	patchFile := fs.String("patch", "", "candidate unified diff to evaluate")
	artifacts := fs.String("artifacts", "", "artifact directory override")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall re-evaluation deadline")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 || *taskFile == "" || *patchFile == "" || *timeout <= 0 {
		fmt.Fprintln(errout, "reeval requires --task and --patch; --timeout must be positive")
		return 1
	}
	root, err := filepath.Abs(*project)
	if err != nil {
		fmt.Fprintln(errout, "project:", err)
		return 1
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		fmt.Fprintln(errout, "project:", err)
		return 1
	}
	c, err := config.Load(*configFile)
	if err != nil {
		fmt.Fprintln(errout, "config:", err)
		return 1
	}
	if *artifacts != "" {
		c.ArtifactsDir = *artifacts
	}
	if !filepath.IsAbs(c.ArtifactsDir) {
		c.ArtifactsDir = filepath.Join(root, c.ArtifactsDir)
	}
	manifest := *taskFile
	if !filepath.IsAbs(manifest) {
		manifest = filepath.Join(root, manifest)
	}
	manifest, err = filepath.Abs(manifest)
	if err != nil {
		fmt.Fprintln(errout, "task:", err)
		return 1
	}
	manifestInfo, err := os.Lstat(manifest)
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode()&os.ModeSymlink != 0 {
		fmt.Fprintln(errout, "task: manifest must be a regular non-symlink file")
		return 1
	}
	rel, err := filepath.Rel(root, manifest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		fmt.Fprintln(errout, "task: manifest must be within the trusted project root")
		return 1
	}
	manifestBytes, err := os.ReadFile(manifest)
	if err != nil {
		fmt.Fprintln(errout, "task:", err)
		return 1
	}
	t, err := task.Load(manifest, root)
	if err != nil {
		fmt.Fprintln(errout, "task:", err)
		return 1
	}
	manifestAfter, err := os.ReadFile(manifest)
	if err != nil || sha256.Sum256(manifestBytes) != sha256.Sum256(manifestAfter) {
		fmt.Fprintln(errout, "task: manifest changed while loading")
		return 1
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	patchBytes, err := readTrustedPatch(*patchFile)
	if err != nil {
		fmt.Fprintln(errout, "patch:", err)
		return 1
	}
	patchText := string(patchBytes)
	if err := validateReevaluationPatch(patchText, t.AllowedFiles); err != nil {
		fmt.Fprintln(errout, "patch:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	connectCtx, ccancel := context.WithTimeout(ctx, 15*time.Second)
	factory, err := dockersandbox.New(connectCtx, dockersandbox.Options{Host: c.DockerHost, Image: c.Image})
	ccancel()
	if err != nil {
		fmt.Fprintln(errout, "Docker:", err)
		return 1
	}
	defer factory.Close()
	report, runErr := runIndependentReevaluation(ctx, factory, t, hex.EncodeToString(manifestDigest[:]), patchBytes, c.ArtifactsDir)
	printJSON(out, report)
	if runErr != nil {
		fmt.Fprintln(errout, "reeval:", runErr)
		return 1
	}
	if report.Verdict != "success" {
		return 2
	}
	return 0
}

func readTrustedPatch(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("patch must be a regular non-symlink file")
	}
	if info.Size() > maxReevaluationPatchBytes {
		return nil, errors.New("patch exceeds 1 MiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxReevaluationPatchBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxReevaluationPatchBytes {
		return nil, errors.New("patch exceeds 1 MiB")
	}
	if !utf8.Valid(b) || strings.IndexByte(string(b), 0) >= 0 {
		return nil, errors.New("patch must be UTF-8 text without NUL bytes")
	}
	return b, nil
}

func validateReevaluationPatch(text string, allowed []string) error {
	if len(text) > maxReevaluationPatchBytes || !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return errors.New("patch is oversized or not valid UTF-8 text")
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	allow := make(map[string]bool, len(allowed))
	for _, p := range allowed {
		allow[p] = true
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	seen := map[string]bool{}
	current := ""
	oldHeader, newHeader := false, false
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			if current != "" && (!oldHeader || !newHeader) {
				return errors.New("each diff section must contain matching --- and +++ headers")
			}
			prefix := "diff --git a/"
			if !strings.HasPrefix(line, prefix) {
				return errors.New("quoted, absolute, or non-standard diff paths are forbidden")
			}
			rest := strings.TrimPrefix(line, prefix)
			sep := strings.Index(rest, " b/")
			if sep <= 0 {
				return errors.New("malformed diff header")
			}
			a, b := rest[:sep], rest[sep+3:]
			if a != b || !task.ValidPath(a) || !allow[a] || strings.ContainsAny(a, " \t\r\n\"") {
				return fmt.Errorf("diff path is not an allowed source file: %q", a)
			}
			if seen[a] {
				return fmt.Errorf("duplicate diff section for %q", a)
			}
			seen[a] = true
			current = a
			oldHeader, newHeader = false, false
			continue
		}
		if current == "" {
			return errors.New("patch must begin with a standard git diff section")
		}
		if strings.HasPrefix(line, "--- ") {
			if line != "--- a/"+current && line != "--- /dev/null" {
				return errors.New("old-file header does not match the allowed diff path")
			}
			oldHeader = true
		}
		if strings.HasPrefix(line, "+++ ") {
			if line != "+++ b/"+current && line != "+++ /dev/null" {
				return errors.New("new-file header does not match the allowed diff path")
			}
			newHeader = true
		}
		if strings.HasPrefix(line, "new file mode ") && line != "new file mode 100644" || strings.HasPrefix(line, "deleted file mode ") && line != "deleted file mode 100644" || strings.HasPrefix(line, "old mode ") || strings.HasPrefix(line, "new mode ") || strings.HasPrefix(line, "rename from ") || strings.HasPrefix(line, "rename to ") || strings.HasPrefix(line, "copy from ") || strings.HasPrefix(line, "copy to ") || strings.HasPrefix(line, "GIT binary patch") || strings.HasPrefix(line, "Binary files ") {
			return errors.New("binary, rename, mode-change, or non-regular-file patches are forbidden")
		}
	}
	if current != "" && (!oldHeader || !newHeader) {
		return errors.New("each diff section must contain matching --- and +++ headers")
	}
	if len(seen) == 0 {
		return errors.New("patch contains no file changes")
	}
	return nil
}

func runIndependentReevaluation(ctx context.Context, f sandbox.Factory, t domain.Task, manifestHash string, patchBytes []byte, artifactRoot string) (report reevaluationReport, retErr error) {
	start := time.Now()
	report = reevaluationReport{SchemaVersion: 1, RuntimeVersion: runner.Version, TaskID: t.ID, TaskRevision: t.Revision, ManifestSHA256: manifestHash, SourceSHA256: t.SourceSHA256, EvaluatorSHA256: t.EvaluatorSHA256, StartedAt: start.UTC(), Verdict: "infra_error", CleanupStatus: "not_started"}
	patchHash := sha256.Sum256(patchBytes)
	report.PatchSHA256 = hex.EncodeToString(patchHash[:])
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return report, err
	}
	report.ReevaluationID = hex.EncodeToString(random)
	dir, err := filepath.Abs(filepath.Join(artifactRoot, "reevaluations", report.ReevaluationID))
	if err != nil {
		return report, err
	}
	report.ArtifactDir = dir
	if err = os.MkdirAll(dir, 0700); err != nil {
		return report, err
	}
	if err = runner.WriteJSON(filepath.Join(dir, "manifest.json"), map[string]any{"schema_version": 1, "reevaluation_id": report.ReevaluationID, "task_id": t.ID, "task_revision": t.Revision, "source_sha256": t.SourceSHA256, "evaluator_sha256": t.EvaluatorSHA256, "patch_sha256": report.PatchSHA256}); err != nil {
		return report, err
	}
	patchPath := filepath.Join(dir, "candidate.patch")
	if err = os.WriteFile(patchPath, patchBytes, 0600); err != nil {
		return report, err
	}
	report.PatchFile = patchPath
	var activeSandbox sandbox.Sandbox
	defer func() {
		if activeSandbox != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Duration(t.Budget.CleanupSeconds)*time.Second)
			if cleanupErr := activeSandbox.Destroy(cleanupCtx); cleanupErr != nil {
				report.CleanupStatus = "failed"
				if retErr == nil {
					retErr = fmt.Errorf("cleanup failed: %w", cleanupErr)
				}
			} else {
				report.CleanupStatus = "succeeded"
			}
			cleanupCancel()
		}
		report.FinishedAt = time.Now().UTC()
		report.WallMS = report.FinishedAt.Sub(start).Milliseconds()
		if report.CleanupStatus == "failed" {
			report.Verdict = "infra_error"
		}
		if retErr != nil {
			report.Error = retErr.Error()
		}
		_ = runner.WriteJSON(filepath.Join(dir, "result.json"), report)
	}()
	prepareCtx, pcancel := context.WithTimeout(ctx, time.Duration(t.Budget.PrepareSeconds)*time.Second)
	info, doctorErr := f.Doctor(prepareCtx)
	pcancel()
	if doctorErr != nil {
		retErr = doctorErr
		return
	}
	if info.OSType != "linux" || (info.Architecture != "amd64" && info.Architecture != "x86_64") || info.ImageID == "" {
		retErr = errors.New("Linux amd64 Engine with a prepared sandbox image is required")
		return
	}
	report.ImageID = info.ImageID
	labels := map[string]string{"agenteval.project": "runtime", "agenteval.run": report.ReevaluationID, "agenteval.attempt": report.ReevaluationID + "-reeval", "agenteval.phase": "reevaluation"}
	createCtx, createCancel := context.WithTimeout(ctx, time.Duration(t.Budget.PrepareSeconds)*time.Second)
	activeSandbox, err = f.Create(createCtx, sandbox.Spec{Image: info.ImageID, Labels: labels, Limits: t.Limits}, evaluator.Inputs(t))
	createCancel()
	if err != nil {
		retErr = err
		return
	}
	evalCtx, evalCancel := context.WithTimeout(ctx, time.Duration(t.Budget.EvalSeconds)*time.Second)
	defer evalCancel()
	result, evalErr := evaluator.Evaluate(evalCtx, activeSandbox, t, string(patchBytes))
	report.Evaluation = result
	report.Verdict = result.Verdict
	report.Score = result.Score
	report.Classification = result.Classification
	if err := runner.WriteJSON(filepath.Join(dir, "evaluation.json"), result); err != nil {
		retErr = err
		report.Verdict = "infra_error"
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "evaluator.log"), []byte(result.Exec.Stdout+"\n--- stderr ---\n"+result.Exec.Stderr), 0600); err != nil {
		retErr = err
		report.Verdict = "infra_error"
		return
	}
	if evalErr != nil {
		retErr = evalErr
		if domain.Reason(evalErr) == "infra_error" || domain.Reason(evalErr) == "timeout" || domain.Reason(evalErr) == "oom" || domain.Reason(evalErr) == "canceled" {
			report.Verdict = domain.Reason(evalErr)
		}
		if result.Verdict == "invalid_patch" {
			report.Verdict = "invalid_patch"
		}
	}
	return report, retErr
}
