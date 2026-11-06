package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	StatusQueued     = "QUEUED"
	StatusRunning    = "RUNNING"
	StatusSucceeded  = "SUCCEEDED"
	StatusFailed     = "FAILED"
	StatusTimedOut   = "TIMED_OUT"
	StatusCanceled   = "CANCELED"
	StatusInfraError = "INFRA_ERROR"
)

var (
	ErrNotFound       = errors.New("job not found")
	ErrConflict       = errors.New("request conflicts with existing job")
	ErrQueueFull      = errors.New("job queue is full")
	ErrNoJob          = errors.New("no queued job available")
	ErrInvalidRequest = errors.New("invalid job request")
	ErrUnavailable    = errors.New("service unavailable")
	ErrStopping       = errors.New("service is stopping")
	ErrLeadershipLost = errors.New("leadership lost")
	ErrAlreadyRunning = errors.New("scheduler is already running")
)

type Preview struct {
	Request      Request      `json:"request"`
	TaskSnapshot TaskSnapshot `json:"task_snapshot"`
}

type Request struct {
	ExpectedSnapshot *TaskSnapshot `json:"expected_snapshot,omitempty"`
	SchemaVersion    int           `json:"schema_version"`
	TaskID           string        `json:"task_id"`
	Agent            string        `json:"agent"`
	Script           string        `json:"script,omitempty"`
	Model            string        `json:"model,omitempty"`
}

type TaskSnapshot struct {
	Backend           string `json:"backend,omitempty"`
	PackageSHA256     string `json:"package_sha256,omitempty"`
	VerifierImageID   string `json:"verifier_image_id,omitempty"`
	TaskRevision      int    `json:"task_revision"`
	ManifestSHA256    string `json:"manifest_sha256"`
	SourceSHA256      string `json:"source_sha256"`
	EvaluatorSHA256   string `json:"evaluator_sha256"`
	ImageID           string `json:"image_id"`
	AgentConfigSHA256 string `json:"agent_config_sha256"`
}

type Job struct {
	ID               string          `json:"id"`
	ProjectID        string          `json:"project_id,omitempty"`
	ExperimentID     string          `json:"experiment_id,omitempty"`
	DatasetVersionID string          `json:"dataset_version_id,omitempty"`
	CreatedBy        string          `json:"created_by,omitempty"`
	Status           string          `json:"status"`
	Request          Request         `json:"request"`
	TaskSnapshot     TaskSnapshot    `json:"task_snapshot"`
	IdempotencyKey   string          `json:"idempotency_key,omitempty"`
	RequestSHA256    string          `json:"request_sha256"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	FinishedAt       *time.Time      `json:"finished_at,omitempty"`
	CancelRequested  bool            `json:"cancel_requested"`
	OwnerToken       string          `json:"-"`
	RunID            string          `json:"run_id,omitempty"`
	AttemptID        string          `json:"attempt_id,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	Reason           string          `json:"reason,omitempty"`
}

type Summary struct {
	ControllerEpoch      int64            `json:"controller_epoch"`
	HeartbeatUnix        int64            `json:"heartbeat_unix"`
	ReconciledContainers int64            `json:"reconciled_containers"`
	ReconcileErrors      int64            `json:"reconcile_errors"`
	Queued               int64            `json:"queued"`
	Running              int64            `json:"running"`
	ByStatus             map[string]int64 `json:"by_status"`
	WorkerLimit          int              `json:"worker_limit"`
	ActiveWorkers        int              `json:"active_workers"`
	Draining             bool             `json:"draining"`
}

type TaskInfo struct {
	Backend  string   `json:"backend,omitempty"`
	ID       string   `json:"task_id"`
	Revision int      `json:"revision"`
	Prompt   string   `json:"prompt"`
	Scripts  []string `json:"scripts"`
}

type EventPage struct {
	Events       []json.RawMessage `json:"events"`
	NextSequence int               `json:"next_sequence"`
	HasMore      bool              `json:"has_more"`
}

type Store interface {
	OwnerID() string
	Submit(context.Context, Request, TaskSnapshot, string, string, int) (Job, bool, error)
	Claim(context.Context, string) (Job, error)
	Get(context.Context, string) (Job, error)
	List(context.Context, int, string) ([]Job, error)
	SetAttempt(context.Context, string, string, string, string) (bool, error)
	Finish(context.Context, string, string, string, json.RawMessage, string) (bool, error)
	Cancel(context.Context, string) (Job, error)
	Recover(context.Context) (int, error)
	Summary(context.Context) (Summary, error)
	Health(context.Context) error
	Close() error
}
