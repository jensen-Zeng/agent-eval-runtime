package jobs

import (
	"context"
	"errors"
	"time"
)

var (
	ErrUnauthorized  = errors.New("unauthorized")
	ErrForbidden     = errors.New("forbidden")
	ErrQuotaExceeded = errors.New("project quota exceeded")
)

type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Admin     bool      `json:"admin"`
	CreatedAt time.Time `json:"created_at"`
}

type Credentials struct {
	User         User   `json:"user"`
	PasswordHash string `json:"-"`
}

type Project struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Role            string    `json:"role"`
	DailyLimit      int       `json:"daily_limit"`
	ConcurrentLimit int       `json:"concurrent_limit"`
	CreatedAt       time.Time `json:"created_at"`
}

type Member struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type DatasetTask struct {
	TaskID   string       `json:"task_id"`
	Snapshot TaskSnapshot `json:"snapshot"`
}

type DatasetVersion struct {
	ID        string        `json:"id"`
	ProjectID string        `json:"project_id"`
	Name      string        `json:"name"`
	Version   string        `json:"version"`
	CreatedBy string        `json:"created_by"`
	Tasks     []DatasetTask `json:"tasks"`
	CreatedAt time.Time     `json:"created_at"`
}

type PreparedJob struct {
	Request  Request      `json:"request"`
	Snapshot TaskSnapshot `json:"task_snapshot"`
}

type Experiment struct {
	ID               string    `json:"id"`
	ProjectID        string    `json:"project_id"`
	DatasetVersionID string    `json:"dataset_version_id"`
	Name             string    `json:"name"`
	CreatedBy        string    `json:"created_by"`
	IdempotencyKey   string    `json:"idempotency_key"`
	CreatedAt        time.Time `json:"created_at"`
	State            string    `json:"state"`
	JobCount         int       `json:"job_count"`
	Jobs             []Job     `json:"jobs,omitempty"`
}

type Usage struct {
	ProjectID       string `json:"project_id"`
	Day             string `json:"day"`
	Reserved        int    `json:"reserved"`
	Used            int    `json:"used"`
	Released        int    `json:"released"`
	Running         int    `json:"running"`
	DailyLimit      int    `json:"daily_limit"`
	ConcurrentLimit int    `json:"concurrent_limit"`
}

// ManagementStore contains the transactional persistence API used by the
// authenticated management surface. Implementations must recheck actor access
// inside each transaction that mutates project-scoped state.
type ManagementStore interface {
	HasUsers(context.Context) (bool, error)
	Bootstrap(context.Context, string, string) (User, error)
	CreateUser(context.Context, string, string, string) (User, error)
	ListUsers(context.Context, string) ([]User, error)
	Credentials(context.Context, string) (Credentials, error)
	CreateSession(context.Context, string, string) error
	Authenticate(context.Context, string) (User, error)
	DeleteSession(context.Context, string) error

	CreateProject(context.Context, string, string, int, int) (Project, error)
	ListProjects(context.Context, string) ([]Project, error)
	GetProject(context.Context, string, string) (Project, error)
	UpdateProjectLimits(context.Context, string, string, int, int) (Project, error)
	ListMembers(context.Context, string, string) ([]Member, error)
	SetMember(context.Context, string, string, string, string) error
	RemoveMember(context.Context, string, string, string) error

	CreateDatasetVersion(context.Context, string, string, string, string, []DatasetTask) (DatasetVersion, error)
	ListDatasetVersions(context.Context, string, string) ([]DatasetVersion, error)
	GetDatasetVersion(context.Context, string, string, string) (DatasetVersion, error)

	SubmitExperiment(context.Context, string, string, string, string, string, []PreparedJob, int) (Experiment, bool, error)
	GetExperiment(context.Context, string, string, string) (Experiment, error)
	ListExperiments(context.Context, string, string) ([]Experiment, error)
	ProjectJobs(context.Context, string, string) ([]Job, error)
	GetProjectJob(context.Context, string, string, string) (Job, error)
	CancelProjectJob(context.Context, string, string, string) (Job, error)
	CancelExperiment(context.Context, string, string, string) (Experiment, error)
	ProjectUsage(context.Context, string, string, string) (Usage, error)
}
