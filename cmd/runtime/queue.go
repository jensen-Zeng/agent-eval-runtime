package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/client"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const defaultQueueURL = "http://127.0.0.1:8787"
const maxBatchBytes = 2 << 20
const maxBatchJobs = 1000

func executeQueue(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		queueUsage(out)
		return 0
	}
	command := args[0]
	fs := flag.NewFlagSet("runtime "+command, flag.ContinueOnError)
	fs.SetOutput(errout)
	urlFlag := fs.String("url", defaultQueueURL, "loopback runtime API URL")
	var taskID, agentName, scriptName, model, idemKey, statusFilter, batchFile, outputFile string
	var limit, after int
	var wait bool
	var timeout time.Duration
	switch command {
	case "submit":
		fs.StringVar(&taskID, "task-id", "", "registered task ID")
		fs.StringVar(&agentName, "agent", "scripted", "scripted or llm")
		fs.StringVar(&scriptName, "script", "", "scripted action sequence (server defaults to success)")
		fs.StringVar(&model, "model", "", "LLM model name")
		fs.StringVar(&idemKey, "idempotency-key", "", "optional idempotency key")
	case "status", "cancel", "tasks":
	case "list":
		fs.IntVar(&limit, "limit", 20, "maximum jobs (1..100)")
		fs.StringVar(&statusFilter, "status", "", "filter by job status")
	case "events":
		fs.IntVar(&after, "after-seq", 0, "exclusive event sequence cursor")
		fs.IntVar(&limit, "limit", 100, "maximum events (1..100)")
	case "batch":
		fs.StringVar(&batchFile, "file", "examples/v0.2-batch.json", "batch input JSON")
		fs.StringVar(&outputFile, "output", "", "progress manifest path (defaults to artifacts/batches/<name>.json)")
		fs.BoolVar(&wait, "wait", false, "wait for submitted jobs to reach a terminal state")
		fs.DurationVar(&timeout, "timeout", 10*time.Minute, "overall batch deadline (Go duration)")
	default:
		fmt.Fprintf(errout, "unknown queue command %q\n", command)
		queueUsage(errout)
		return 1
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	api, err := client.New(*urlFlag)
	if err != nil {
		fmt.Fprintln(errout, "URL:", err)
		return 1
	}
	defer api.Close()
	switch command {
	case "submit":
		if fs.NArg() != 0 || strings.TrimSpace(taskID) == "" || len(taskID) > 80 || (agentName != "scripted" && agentName != "llm") || len(scriptName) > 80 || len(model) > 256 || (agentName == "llm" && scriptName != "") || (agentName == "scripted" && model != "") || !jobs.ValidIdempotencyKey(idemKey) {
			fmt.Fprintln(errout, "submit request fields are invalid; use --task-id and a compatible --agent, --script, and --model")
			return 1
		}
		job, reused, err := api.Submit(ctx, jobs.Request{SchemaVersion: 1, TaskID: taskID, Agent: agentName, Script: scriptName, Model: model}, idemKey)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		printJSON(out, map[string]any{"job": job, "reused": reused})
		return 0
	case "status":
		if fs.NArg() != 1 {
			fmt.Fprintln(errout, "status requires <job-id>")
			return 1
		}
		job, err := api.Get(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		printJSON(out, job)
		return 0
	case "list":
		if fs.NArg() != 0 || limit < 1 || limit > 100 {
			fmt.Fprintln(errout, "list --limit must be between 1 and 100")
			return 1
		}
		list, err := api.List(ctx, limit, statusFilter)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		printJSON(out, map[string]any{"jobs": list})
		return 0
	case "cancel":
		if fs.NArg() != 1 {
			fmt.Fprintln(errout, "cancel requires <job-id>")
			return 1
		}
		job, err := api.Cancel(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		printJSON(out, job)
		return 0
	case "events":
		if fs.NArg() != 1 || after < 0 || limit < 1 || limit > 100 {
			fmt.Fprintln(errout, "events requires <job-id>; after-seq must be non-negative and limit 1..100")
			return 1
		}
		page, err := api.Events(ctx, fs.Arg(0), after, limit)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		printJSON(out, page)
		return 0
	case "tasks":
		if fs.NArg() != 0 {
			fmt.Fprintln(errout, "tasks accepts no positional arguments")
			return 1
		}
		tasks, err := api.Tasks(ctx)
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		printJSON(out, map[string]any{"tasks": tasks})
		return 0
	case "batch":
		if fs.NArg() != 0 || timeout <= 0 {
			fmt.Fprintln(errout, "batch accepts no positional arguments; --timeout must be positive")
			return 1
		}
		return executeBatch(ctx, api, batchFile, outputFile, wait, timeout, out, errout)
	}
	return 1
}

func queueUsage(w io.Writer) {
	fmt.Fprintln(w, "Legacy queue commands for pre-v1.4 services (v1.4 uses the web UI or /v1/manage API):")
	fmt.Fprintln(w, "  submit --task-id ID [--agent scripted|llm] [--script NAME] [--model MODEL] [--idempotency-key KEY]")
	fmt.Fprintln(w, "  status <job-id> | list [--limit 20] [--status STATUS] | cancel <job-id>")
	fmt.Fprintln(w, "  events [--after-seq 0] [--limit 100] <job-id> | tasks")
	fmt.Fprintln(w, "  batch [--file examples/v0.2-batch.json] [--output PATH] [--wait] [--timeout 10m]")
}

type batchInput struct {
	SchemaVersion int         `json:"schema_version"`
	Name          string      `json:"name"`
	Jobs          []batchItem `json:"jobs"`
}
type batchItem struct {
	Request        jobs.Request `json:"request"`
	IdempotencyKey string       `json:"idempotency_key"`
}
type batchSubmission struct {
	Request        jobs.Request `json:"request"`
	IdempotencyKey string       `json:"idempotency_key"`
	Reused         bool         `json:"reused,omitempty"`
	Job            *jobs.Job    `json:"job,omitempty"`
	Error          string       `json:"error,omitempty"`
}
type batchSummary struct {
	Total                  int      `json:"total"`
	Submitted              int      `json:"submitted"`
	SubmissionErrors       int      `json:"submission_errors"`
	Unsubmitted            int      `json:"unsubmitted"`
	Terminal               int      `json:"terminal"`
	Success                int      `json:"success"`
	Failure                int      `json:"failure"`
	Unknown                int      `json:"unknown"`
	SuccessRate            *float64 `json:"success_rate,omitempty"`
	InfraErrors            int      `json:"infra_errors"`
	TokenTotal             int64    `json:"token_total"`
	TokenEstimatedTotal    int64    `json:"token_estimated_total"`
	TokenEstimatedJobs     int      `json:"token_estimated_jobs"`
	TokenUnknown           int      `json:"token_unknown"`
	WallMS                 int64    `json:"wall_ms"`
	WallUnknown            int      `json:"wall_unknown"`
	CPUSeconds             float64  `json:"cpu_seconds"`
	CPUUnknown             int      `json:"cpu_unknown"`
	MemoryPeakSampledBytes uint64   `json:"memory_peak_sampled_bytes"`
	MemoryUnknown          int      `json:"memory_unknown"`
	ResourceSamples        int64    `json:"resource_samples"`
	ResourceSampleErrors   int64    `json:"resource_sample_errors"`
	OOMCount               int      `json:"oom_count"`
}
type batchManifest struct {
	SchemaVersion int               `json:"schema_version"`
	Name          string            `json:"name"`
	StartedAt     time.Time         `json:"started_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Complete      bool              `json:"complete"`
	Submissions   []batchSubmission `json:"submissions"`
	Summary       batchSummary      `json:"summary"`
}

func executeBatch(parent context.Context, api *client.Client, inputPath, outputPath string, wait bool, timeout time.Duration, out, errout io.Writer) int {
	input, err := readBatch(inputPath)
	if err != nil {
		fmt.Fprintln(errout, "batch:", err)
		return 1
	}
	if outputPath == "" {
		outputPath = filepath.Join("artifacts", "batches", input.Name+".json")
	}
	inputAbs, inErr := filepath.Abs(inputPath)
	outputAbs, outErr := filepath.Abs(outputPath)
	if inErr != nil || outErr != nil || strings.EqualFold(filepath.Clean(inputAbs), filepath.Clean(outputAbs)) {
		fmt.Fprintln(errout, "batch input and output must be different valid paths")
		return 1
	}
	if inputInfo, err := os.Stat(inputAbs); err == nil {
		if outputInfo, err := os.Stat(outputAbs); err == nil && os.SameFile(inputInfo, outputInfo) {
			fmt.Fprintln(errout, "batch input and output refer to the same file")
			return 1
		}
	}
	manifest := batchManifest{SchemaVersion: 1, Name: input.Name, StartedAt: time.Now().UTC(), Submissions: make([]batchSubmission, 0, len(input.Jobs))}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	if err := writeBatchManifest(outputPath, &manifest, len(input.Jobs)); err != nil {
		fmt.Fprintln(errout, "manifest:", err)
		return 1
	}
	var persistentErr error
	for _, item := range input.Jobs {
		entry := batchSubmission{Request: item.Request, IdempotencyKey: item.IdempotencyKey}
		for {
			job, reused, submitErr := api.Submit(ctx, item.Request, item.IdempotencyKey)
			if submitErr == nil {
				entry.Job, entry.Reused = &job, reused
				break
			}
			var apiErr *client.APIError
			if wait && errors.As(submitErr, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests {
				delay := apiErr.RetryAfter
				if delay <= 0 {
					delay = time.Second
				}
				if time.Until(deadline) < delay {
					entry.Error = "queue remained full until batch deadline"
					break
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					entry.Error = "submission stopped before queue capacity became available"
				case <-timer.C:
					continue
				}
			} else {
				entry.Error = submitErr.Error()
			}
			break
		}
		manifest.Submissions = append(manifest.Submissions, entry)
		manifest.Summary = summarizeBatch(manifest.Submissions, len(input.Jobs))
		if err := writeBatchManifest(outputPath, &manifest, len(input.Jobs)); err != nil {
			persistentErr = err
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if wait && ctx.Err() == nil && persistentErr == nil {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for hasActiveSubmissions(manifest.Submissions) {
			select {
			case <-ctx.Done():
				goto finished
			case <-ticker.C:
				for i := range manifest.Submissions {
					entry := &manifest.Submissions[i]
					if entry.Job == nil || !isActive(entry.Job.Status) {
						continue
					}
					job, getErr := api.Get(ctx, entry.Job.ID)
					if getErr != nil {
						entry.Error = getErr.Error()
						continue
					}
					entry.Job, entry.Error = &job, ""
				}
				if err := writeBatchManifest(outputPath, &manifest, len(input.Jobs)); err != nil {
					persistentErr = err
					goto finished
				}
			}
		}
	}
finished:
	manifest.Summary = summarizeBatch(manifest.Submissions, len(input.Jobs))
	manifest.Complete = len(manifest.Submissions) == len(input.Jobs) && manifest.Summary.SubmissionErrors == 0 && manifest.Summary.Unknown == 0
	if err := writeBatchManifest(outputPath, &manifest, len(input.Jobs)); err != nil && persistentErr == nil {
		persistentErr = err
	}
	printJSON(out, manifest)
	if persistentErr == nil {
		fmt.Fprintf(errout, "batch progress saved to %s; submitted job IDs are in the manifest\n", outputPath)
	} else {
		fmt.Fprintln(errout, "could not persist the latest progress; submitted job IDs are in stdout")
	}
	if persistentErr != nil {
		fmt.Fprintln(errout, "manifest:", persistentErr)
		return 1
	}
	if ctx.Err() != nil {
		fmt.Fprintf(errout, "batch stopped: %v; submitted jobs remain on the server\n", ctx.Err())
		return 1
	}
	for _, entry := range manifest.Submissions {
		if entry.Error != "" {
			return 1
		}
	}
	if manifest.Summary.SubmissionErrors > 0 || manifest.Summary.Unsubmitted > 0 {
		return 1
	}
	if !wait {
		return 0
	}
	if manifest.Summary.Unknown > 0 {
		return 1
	}
	if manifest.Summary.InfraErrors > 0 {
		return 1
	}
	if manifest.Summary.Failure > 0 {
		return 2
	}
	return 0
}

func readBatch(path string) (batchInput, error) {
	var input batchInput
	file, err := os.Open(path)
	if err != nil {
		return input, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBatchBytes+1))
	if err != nil {
		return input, err
	}
	if len(data) > maxBatchBytes {
		return input, errors.New("input exceeds the 2 MiB limit")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		return input, fmt.Errorf("invalid JSON: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return input, errors.New("input must contain exactly one JSON value")
	}
	if input.SchemaVersion != 1 || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`).MatchString(input.Name) || len(input.Jobs) == 0 || len(input.Jobs) > maxBatchJobs {
		return input, errors.New("schema_version must be 1, name must be a safe 1..80 character identifier, and jobs must contain 1..1000 entries")
	}
	keys := make(map[string]struct{}, len(input.Jobs))
	for i, item := range input.Jobs {
		if !jobs.ValidIdempotencyKey(item.IdempotencyKey) || item.IdempotencyKey == "" {
			return input, fmt.Errorf("jobs[%d].idempotency_key is invalid or empty", i)
		}
		if _, exists := keys[item.IdempotencyKey]; exists {
			return input, fmt.Errorf("jobs[%d].idempotency_key is duplicated", i)
		}
		keys[item.IdempotencyKey] = struct{}{}
		req := item.Request
		if req.SchemaVersion != 0 && req.SchemaVersion != 1 || strings.TrimSpace(req.TaskID) == "" || len(req.TaskID) > 80 {
			return input, fmt.Errorf("jobs[%d].request fields are invalid", i)
		}
		if req.Agent == "" {
			req.Agent = "scripted"
		}
		if req.Agent != "scripted" && req.Agent != "llm" || len(req.Script) > 80 || len(req.Model) > 256 || (req.Agent == "llm" && req.Script != "") || (req.Agent == "scripted" && req.Model != "") {
			return input, fmt.Errorf("jobs[%d].request agent/script/model fields are incompatible", i)
		}
		input.Jobs[i].Request = req
	}
	return input, nil
}

func writeBatchManifest(path string, manifest *batchManifest, total int) error {
	manifest.UpdatedAt = time.Now().UTC()
	manifest.Summary = summarizeBatch(manifest.Submissions, total)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".batch-*.tmp")
	if err != nil {
		return err
	}
	tmpName, ok := tmp.Name(), false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomic manifest replacement failed (previous manifest retained): %w", err)
	}
	ok = true
	return nil
}

func summarizeBatch(entries []batchSubmission, total int) batchSummary {
	s := batchSummary{Total: total, Unsubmitted: max(0, total-len(entries))}
	for _, entry := range entries {
		if entry.Job == nil {
			s.SubmissionErrors++
			continue
		}
		job := entry.Job
		s.Submitted++
		if isActive(job.Status) {
			s.Unknown++
			continue
		}
		s.Terminal++
		if job.Status == jobs.StatusSucceeded {
			s.Success++
		} else {
			s.Failure++
		}
		if job.Status == jobs.StatusInfraError {
			s.InfraErrors++
		}
		var report struct {
			RunID      string        `json:"run_id"`
			StartedAt  *time.Time    `json:"started_at"`
			FinishedAt *time.Time    `json:"finished_at"`
			WallMS     *int64        `json:"wall_ms"`
			Usage      *domain.Usage `json:"usage"`
			Resources  map[string]struct {
				CPUSeconds   *float64 `json:"cpu_seconds"`
				MemoryPeak   *uint64  `json:"memory_peak_sampled_bytes"`
				Samples      int      `json:"samples"`
				SampleErrors int      `json:"sample_errors"`
				OOM          bool     `json:"oom_killed"`
			} `json:"resources"`
		}
		if len(job.Result) == 0 || string(job.Result) == "null" || json.Unmarshal(job.Result, &report) != nil {
			s.TokenUnknown++
			s.WallUnknown++
			s.CPUUnknown++
			s.MemoryUnknown++
			continue
		}
		if report.Usage != nil && (report.Usage.Quality == "known" || report.Usage.Quality == "reported") {
			s.TokenTotal += int64(report.Usage.TotalTokens)
		} else {
			s.TokenUnknown++
			if report.Usage != nil && report.Usage.Quality == "estimated" {
				s.TokenEstimatedTotal += int64(report.Usage.TotalTokens)
				s.TokenEstimatedJobs++
			}
		}
		if report.RunID == "" || report.StartedAt == nil || report.StartedAt.IsZero() || report.FinishedAt == nil || report.FinishedAt.IsZero() || report.WallMS == nil {
			s.WallUnknown++
		} else {
			s.WallMS += *report.WallMS
		}
		cpuComplete, memComplete := len(report.Resources) > 0, len(report.Resources) > 0
		for _, resource := range report.Resources {
			s.ResourceSamples += int64(resource.Samples)
			s.ResourceSampleErrors += int64(resource.SampleErrors)
			if resource.OOM {
				s.OOMCount++
			}
			if resource.CPUSeconds == nil || math.IsNaN(*resource.CPUSeconds) || math.IsInf(*resource.CPUSeconds, 0) {
				cpuComplete = false
			} else {
				s.CPUSeconds += *resource.CPUSeconds
			}
			if resource.MemoryPeak != nil {
				if *resource.MemoryPeak > s.MemoryPeakSampledBytes {
					s.MemoryPeakSampledBytes = *resource.MemoryPeak
				}
			} else {
				memComplete = false
			}
		}
		if !cpuComplete {
			s.CPUUnknown++
		}
		if !memComplete {
			s.MemoryUnknown++
		}
	}
	if s.Terminal > 0 {
		rate := float64(s.Success) / float64(s.Terminal)
		s.SuccessRate = &rate
	}
	return s
}

func isActive(status string) bool { return status == jobs.StatusQueued || status == jobs.StatusRunning }
func hasActiveSubmissions(entries []batchSubmission) bool {
	for _, entry := range entries {
		if entry.Job != nil && isActive(entry.Job.Status) {
			return true
		}
	}
	return false
}
