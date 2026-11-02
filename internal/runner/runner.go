// Package runner implements one synchronous, bounded attempt and trusted evaluation.
package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/evaluator"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/patch"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const Version = "1.3.0"

type ResourceSummary struct {
	CPUSeconds             *float64 `json:"cpu_seconds"`
	MemoryPeakSampledBytes *uint64  `json:"memory_peak_sampled_bytes"`
	Samples                int      `json:"samples"`
	SampleErrors           int      `json:"sample_errors"`
	OOMKilled              bool     `json:"oom_killed"`
}
type Report struct {
	Backend           string                      `json:"backend,omitempty"`
	PackageSHA256     string                      `json:"package_sha256,omitempty"`
	VerifierImageID   string                      `json:"verifier_image_id,omitempty"`
	SubmissionSHA256  string                      `json:"submission_sha256,omitempty"`
	Metadata          map[string]string           `json:"metadata,omitempty"`
	Environment       domain.ExecutionEnvironment `json:"environment,omitempty"`
	SchemaVersion     int                         `json:"schema_version"`
	Version           string                      `json:"runtime_version"`
	RunID             string                      `json:"run_id"`
	AttemptID         string                      `json:"attempt_id"`
	TaskID            string                      `json:"task_id"`
	TaskRevision      int                         `json:"task_revision"`
	Status            string                      `json:"status"`
	Verdict           string                      `json:"verdict"`
	Reason            string                      `json:"reason"`
	StartedAt         time.Time                   `json:"started_at"`
	FinishedAt        time.Time                   `json:"finished_at"`
	WallMS            int64                       `json:"wall_ms"`
	PhaseMS           map[string]int64            `json:"phase_ms"`
	ImageID           string                      `json:"image_id,omitempty"`
	SandboxIDs        []string                    `json:"sandbox_ids"`
	SourceSHA256      string                      `json:"source_sha256"`
	EvaluatorSHA256   string                      `json:"evaluator_sha256"`
	PatchSHA256       string                      `json:"patch_sha256,omitempty"`
	Agent             string                      `json:"agent"`
	AgentConfigSHA256 string                      `json:"agent_config_sha256"`
	Budget            domain.Budget               `json:"budget"`
	Limits            sandbox.Limits              `json:"limits"`
	Usage             domain.Usage                `json:"usage"`
	Evaluation        *evaluator.Result           `json:"evaluation,omitempty"`
	Resources         map[string]ResourceSummary  `json:"resources"`
	CleanupStatus     string                      `json:"cleanup_status"`
	CleanupErrors     []string                    `json:"cleanup_errors,omitempty"`
	ArtifactDir       string                      `json:"artifact_dir"`
	Artifacts         map[string]string           `json:"artifacts"`
	Error             string                      `json:"error,omitempty"`
}

type Options struct {
	Image, ArtifactsDir, AgentName string
	AgentConfig                    any
	Labels                         map[string]string
	OnStarted                      func(runID, attemptID string) error
}

func Run(parent context.Context, f sandbox.Factory, t domain.Task, a domain.Agent, opts Options) (report Report, retErr error) {
	started := time.Now()
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return report, err
	}
	id := hex.EncodeToString(idBytes)
	abs, err := filepath.Abs(filepath.Join(opts.ArtifactsDir, id))
	if err != nil {
		return report, err
	}
	attemptDir := filepath.Join(abs, "attempts", id+"-1")
	if err = os.MkdirAll(attemptDir, 0700); err != nil {
		return report, err
	}
	configBytes, _ := json.Marshal(opts.AgentConfig)
	configHash := sha256.Sum256(configBytes)
	report = Report{Metadata: t.Metadata, Environment: t.Environment, SchemaVersion: 1, Version: Version, RunID: id, AttemptID: id + "-1", TaskID: t.ID, TaskRevision: t.Revision, Status: "PREPARING", StartedAt: started.UTC(), PhaseMS: map[string]int64{}, SourceSHA256: t.SourceSHA256, EvaluatorSHA256: t.EvaluatorSHA256, Agent: opts.AgentName, AgentConfigSHA256: hex.EncodeToString(configHash[:]), Budget: t.Budget, Limits: t.Limits, Resources: map[string]ResourceSummary{}, CleanupStatus: "pending", ArtifactDir: abs, Artifacts: map[string]string{}}
	if t.IsHarbor() {
		report.Backend = "harbor"
		report.PackageSHA256 = t.Harbor.PackageSHA256
		report.VerifierImageID = t.Harbor.VerifierImage
	}
	trace, err := os.OpenFile(filepath.Join(attemptDir, "trace.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return report, err
	}
	report.Artifacts["trace"] = "attempts/" + report.AttemptID + "/trace.jsonl"
	seq := 0
	traceBytes := 0
	var traceErr error
	emit := func(phase, event string, data any) {
		seq++
		e := domain.Event{Sequence: seq, Time: time.Now().UTC(), Phase: phase, Type: event, Data: data}
		b, err := json.Marshal(e)
		if err != nil {
			traceErr = err
			return
		}
		if len(b) > 256<<10 {
			e.Data = map[string]any{"truncated": true, "original_bytes": len(b)}
			b, _ = json.Marshal(e)
		}
		if traceBytes+len(b) > 10<<20 {
			traceErr = fmt.Errorf("trace size limit exceeded")
			return
		}
		_, err = trace.Write(append(b, '\n'))
		if err != nil {
			traceErr = err
		}
		traceBytes += len(b) + 1
	}
	runCtx, cancel := context.WithTimeout(parent, time.Duration(t.Budget.RunSeconds)*time.Second)
	defer cancel()
	var created []sandbox.Sandbox
	cleanup := func() {
		begin := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(t.Budget.CleanupSeconds)*time.Second)
		defer cancel()
		for i := len(created) - 1; i >= 0; i-- {
			if err := created[i].Destroy(ctx); err != nil {
				report.CleanupErrors = append(report.CleanupErrors, created[i].ID()+": "+err.Error())
			}
		}
		report.PhaseMS["cleanup"] = time.Since(begin).Milliseconds()
		report.CleanupStatus = "succeeded"
		if len(report.CleanupErrors) > 0 {
			report.CleanupStatus = "failed"
		}
		emit("cleanup", "cleanup_finished", map[string]any{"status": report.CleanupStatus})
	}
	// Finalization runs for both success and failures. Cleanup is independent of a canceled run.
	defer func() {
		cleanup()
		if report.CleanupStatus == "failed" {
			report.Verdict = "infra_error"
			report.Reason = "cleanup_error"
			retErr = fmt.Errorf("cleanup failed; see result.json")
		}
		artifactError := func(err error) {
			if err != nil {
				retErr = err
				report.Status = "INFRA_ERROR"
				report.Verdict = "infra_error"
				report.Reason = "artifact_error"
			}
		}
		report.FinishedAt = time.Now().UTC()
		report.WallMS = time.Since(started).Milliseconds()
		report.Status = status(report.Verdict)
		emit("finalize", "run_finalized", map[string]any{"status": report.Status, "verdict": report.Verdict})
		artifactError(traceErr)
		artifactError(trace.Sync())
		artifactError(trace.Close())
		manifest := map[string]any{"schema_version": 1, "runtime_version": Version, "run_id": id, "task_id": t.ID, "task_revision": t.Revision, "image_id": report.ImageID, "source_sha256": t.SourceSHA256, "evaluator_sha256": t.EvaluatorSHA256, "patch_sha256": report.PatchSHA256, "agent_config_sha256": report.AgentConfigSHA256, "budget": t.Budget, "limits": t.Limits, "metadata": t.Metadata, "environment": t.Environment, "backend": report.Backend, "package_sha256": report.PackageSHA256, "verifier_image_id": report.VerifierImageID, "submission_sha256": report.SubmissionSHA256}
		artifactError(WriteJSON(filepath.Join(abs, "manifest.json"), manifest))
		md := fmt.Sprintf("# Run %s\n\nTask: %s r%d\n\nStatus: %s\n\nVerdict: %s (%s)\n\nDuration: %d ms\n\nCleanup: %s\n\nSee result.json and attempts/%s/trace.jsonl for evidence.\n", id, t.ID, t.Revision, report.Status, report.Verdict, report.Reason, report.WallMS, report.CleanupStatus, report.AttemptID)
		artifactError(writeFile(filepath.Join(abs, "report.md"), []byte(md)))
		if report.CleanupStatus == "failed" && retErr == nil {
			retErr = fmt.Errorf("cleanup failed; see result.json")
		}
		if retErr != nil {
			report.Error = retErr.Error()
		}
		if err := WriteJSON(filepath.Join(abs, "result.json"), report); err != nil {
			retErr = fmt.Errorf("cannot persist result: %w", err)
			report.Status = "INFRA_ERROR"
			report.Verdict = "infra_error"
			report.Reason = "artifact_error"
		}
	}()
	fail := func(err error, phase string) {
		report.Reason = domain.Reason(err)
		report.Verdict = report.Reason
		if strings.HasPrefix(report.Reason, "provider_") || report.Reason == "configuration_error" || report.Reason == "artifact_error" || report.Reason == "malformed_response" {
			report.Verdict = "infra_error"
		}
		if report.Reason == "step_budget" || report.Reason == "token_budget" {
			report.Verdict = "agent_budget_exhausted"
		}
		emit(phase, "phase_failed", map[string]any{"reason": report.Reason})
		retErr = err
	}
	emit("prepare", "run_started", map[string]any{"task_id": t.ID, "budget": t.Budget})
	if opts.OnStarted != nil {
		if err := opts.OnStarted(report.RunID, report.AttemptID); err != nil {
			fail(domain.Fail("infra_error", fmt.Errorf("persist queue attempt: %w", err)), "prepare")
			return
		}
	}
	prepareStart := time.Now()
	prepareCtx, pcancel := context.WithTimeout(runCtx, time.Duration(t.Budget.PrepareSeconds)*time.Second)
	info, err := f.Doctor(prepareCtx)
	if err != nil {
		pcancel()
		fail(err, "prepare")
		return
	}
	report.ImageID = info.ImageID
	if strings.HasPrefix(opts.Image, "sha256:") {
		imageDigest, digestErr := hex.DecodeString(strings.TrimPrefix(opts.Image, "sha256:"))
		if digestErr != nil || len(imageDigest) != sha256.Size {
			pcancel()
			fail(domain.Fail("infra_error", fmt.Errorf("invalid frozen image ID")), "prepare")
			return
		}
		report.ImageID = opts.Image
	}
	if t.IsHarbor() {
		if strings.HasPrefix(opts.Image, "sha256:") && opts.Image != t.Harbor.AgentImage {
			pcancel()
			fail(domain.Fail("infra_error", fmt.Errorf("Harbor agent image differs from frozen task")), "prepare")
			return
		}
		report.ImageID = t.Harbor.AgentImage
	}
	if info.OSType != "linux" || (info.Architecture != "x86_64" && info.Architecture != "amd64") {
		pcancel()
		fail(domain.Fail("infra_error", fmt.Errorf("Linux amd64 Engine required")), "prepare")
		return
	}
	if report.ImageID == "" {
		pcancel()
		fail(domain.Fail("infra_error", fmt.Errorf("sandbox image missing; run runtime prepare")), "prepare")
		return
	}
	labels := make(map[string]string, len(opts.Labels)+4)
	for key, value := range opts.Labels {
		labels[key] = value
	}
	labels["agenteval.project"] = "runtime"
	labels["agenteval.run"] = id
	labels["agenteval.attempt"] = report.AttemptID
	labels["agenteval.phase"] = "agent"
	spec := sandbox.Spec{Image: report.ImageID, Labels: labels, Limits: t.Limits}
	seed := t.Source
	if t.IsHarbor() {
		spec.Workdir = t.Workdir()
		spec.Harbor = true
		spec.ImageWorkspace = true
		spec.Env = t.CommandEnvironment()
		seed = nil
	}
	execution, err := f.Create(prepareCtx, spec, seed)
	pcancel()
	report.PhaseMS["prepare"] = time.Since(prepareStart).Milliseconds()
	if err != nil {
		fail(err, "prepare")
		return
	}
	created = append(created, execution)
	report.SandboxIDs = append(report.SandboxIDs, execution.ID())
	emit("prepare", "sandbox_created", map[string]any{"id": execution.ID(), "image": report.ImageID})
	stopStats := monitor(execution)
	agentStart := time.Now()
	report.Status = "RUNNING"
	deadline, _ := runCtx.Deadline()
	reserve := time.Duration(t.Budget.CollectSeconds+t.Budget.EvalSeconds+15) * time.Second
	agentDeadline := deadline.Add(-reserve)
	if t.IsHarbor() {
		// Cleanup is outside the Agent allowance in Harbor's declared budget.
		seconds := t.Budget.RunSeconds - t.Budget.EvalSeconds - t.Budget.CollectSeconds - t.Budget.CleanupSeconds
		if declared := agentStart.Add(time.Duration(seconds) * time.Second); declared.Before(agentDeadline) {
			agentDeadline = declared
		}
	}
	agentCtx, acancel := context.WithDeadline(runCtx, agentDeadline)
	ar, err := a.Run(agentCtx, execution, t, emit)
	acancel()
	report.Usage = ar.Usage
	report.PhaseMS["agent"] = time.Since(agentStart).Milliseconds()
	report.Resources["agent"] = stopStats()
	if err != nil {
		if domain.Reason(err) == "oom" || domain.Reason(err) == "resource_limit" {
			r := report.Resources["agent"]
			r.OOMKilled = true
			report.Resources["agent"] = r
		}
		fail(err, "agent")
		return
	}
	collectStart := time.Now()
	collectCtx, ccancel := context.WithTimeout(runCtx, time.Duration(t.Budget.CollectSeconds)*time.Second)
	current, err := execution.Snapshot(collectCtx)
	ccancel()
	report.PhaseMS["collect"] = time.Since(collectStart).Milliseconds()
	if err != nil {
		fail(err, "collect")
		return
	}
	var candidate string
	var modes map[string]uint32
	if t.IsHarbor() {
		if source, ok := execution.(sandbox.ModeSnapshotter); ok {
			modes = source.SnapshotModes()
		}
		submission, archiveErr := submissionArchive(current, modes)
		if archiveErr != nil {
			fail(domain.Fail("invalid_submission", archiveErr), "collect")
			return
		}
		h := sha256.Sum256(submission)
		report.SubmissionSHA256 = hex.EncodeToString(h[:])
		name := "submission.tar.gz"
		if err = writeFile(filepath.Join(attemptDir, name), submission); err != nil {
			fail(domain.Fail("artifact_error", err), "collect")
			return
		}
		report.Artifacts["submission"] = "attempts/" + report.AttemptID + "/" + name
		emit("collect", "submission_collected", map[string]any{"sha256": report.SubmissionSHA256, "files": len(current), "archive_bytes": len(submission)})
	} else {
		candidate, err = patch.Generate(t.Source, current, t.AllowedFiles)
		if err != nil {
			fail(domain.Fail("invalid_patch", err), "collect")
			return
		}
		ph := sha256.Sum256([]byte(candidate))
		report.PatchSHA256 = hex.EncodeToString(ph[:])
		patchFile := filepath.Join(attemptDir, "candidate.patch")
		if err = writeFile(patchFile, []byte(candidate)); err != nil {
			fail(domain.Fail("artifact_error", err), "collect")
			return
		}
		report.Artifacts["patch"] = "attempts/" + report.AttemptID + "/candidate.patch"
		emit("collect", "patch_collected", map[string]any{"sha256": report.PatchSHA256, "bytes": len(candidate)})
	}
	// The task sandbox is never reused for evaluation. It also carries no trusted tests.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Duration(t.Budget.CleanupSeconds)*time.Second)
	err = execution.Destroy(closeCtx)
	closeCancel()
	if err != nil {
		fail(domain.Fail("infra_error", fmt.Errorf("cannot release execution sandbox: %w", err)), "collect")
		return
	}
	created = created[1:] // The successfully released task resource needs no second daemon request.
	labels["agenteval.phase"] = "evaluation"
	spec.Labels = labels
	evalStart := time.Now()
	evalCtx, ecancel := context.WithTimeout(runCtx, time.Duration(t.Budget.EvalSeconds)*time.Second)
	defer ecancel()
	evaluationInputs := evaluator.Inputs(t)
	if t.IsHarbor() {
		spec.Image = t.Harbor.VerifierImage
		spec.WritableDirs = t.Harbor.VerifierTmpfs
		spec.Env = t.Harbor.VerifierEnv
		spec.FileModes = modes
		evaluationInputs = current
	}
	evalSandbox, err := f.Create(evalCtx, spec, evaluationInputs)
	if err != nil {
		fail(err, "eval")
		return
	}
	created = append(created, evalSandbox)
	report.SandboxIDs = append(report.SandboxIDs, evalSandbox.ID())
	emit("eval", "evaluation_started", map[string]any{"sandbox_id": evalSandbox.ID(), "evaluator_sha256": t.EvaluatorSHA256})
	stopEval := monitor(evalSandbox)
	er, err := evaluator.Evaluate(evalCtx, evalSandbox, t, candidate)
	report.Resources["evaluation"] = stopEval()
	report.PhaseMS["eval"] = time.Since(evalStart).Milliseconds()
	report.Evaluation = &er
	if writeErr := WriteJSON(filepath.Join(attemptDir, "evaluation.json"), er); writeErr != nil {
		fail(domain.Fail("artifact_error", writeErr), "eval")
		return
	}
	if writeErr := writeFile(filepath.Join(attemptDir, "evaluator.log"), []byte(er.Exec.Stdout+"\n--- stderr ---\n"+er.Exec.Stderr)); writeErr != nil {
		fail(domain.Fail("artifact_error", writeErr), "eval")
		return
	}
	report.Artifacts["evaluation"] = "attempts/" + report.AttemptID + "/evaluation.json"
	report.Artifacts["evaluator_log"] = "attempts/" + report.AttemptID + "/evaluator.log"
	if err != nil {
		fail(err, "eval")
		return
	}
	report.Verdict = er.Verdict
	report.Reason = er.Verdict
	emit("eval", "evaluation_finished", map[string]any{"verdict": er.Verdict, "passed_tests": er.Passed, "missing_tests": er.Missing})
	return
}

func status(v string) string {
	switch v {
	case "success":
		return "SUCCEEDED"
	case "timeout":
		return "TIMED_OUT"
	case "canceled":
		return "CANCELED"
	case "infra_error", "provider_error", "artifact_error", "":
		return "INFRA_ERROR"
	default:
		return "FAILED"
	}
}

func WriteJSON(file string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(file, append(b, '\n'))
}
func writeFile(file string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), ".result-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, file)
}

func monitor(s sandbox.Sandbox) func() ResourceSummary {
	var mu sync.Mutex
	summary := ResourceSummary{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	sample := func() {
		c, cc := context.WithTimeout(ctx, 2*time.Second)
		defer cc()
		st, err := s.Stats(c)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				summary.SampleErrors++
			}
			return
		}
		summary.Samples++
		cpu := st.CPUSeconds
		if summary.CPUSeconds == nil || cpu > *summary.CPUSeconds {
			summary.CPUSeconds = &cpu
		}
		mem := st.MemoryBytes
		if summary.MemoryPeakSampledBytes == nil || mem > *summary.MemoryPeakSampledBytes {
			summary.MemoryPeakSampledBytes = &mem
		}
		summary.OOMKilled = summary.OOMKilled || st.OOMKilled
	}
	go func() {
		defer close(done)
		sample()
		timer := time.NewTicker(500 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				sample()
			}
		}
	}()
	return func() ResourceSummary { cancel(); <-done; mu.Lock(); defer mu.Unlock(); return summary }
}
