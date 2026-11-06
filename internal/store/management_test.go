package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

type managementFixture struct {
	store    *Postgres
	db       *sql.DB
	dsn      string
	admin    jobs.User
	project  jobs.Project
	dataset  jobs.DatasetVersion
	tasks    []jobs.DatasetTask
	prepared []jobs.PreparedJob
}

func newManagementFixture(t *testing.T, dailyLimit, concurrentLimit, taskCount int) *managementFixture {
	t.Helper()
	store, _, dsn := integrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := store.Bootstrap(ctx, "admin-user", "test-password-hash")
	if err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	project, err := store.CreateProject(ctx, admin.ID, "project", dailyLimit, concurrentLimit)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	tasks := make([]jobs.DatasetTask, taskCount)
	prepared := make([]jobs.PreparedJob, taskCount)
	for i := 0; i < taskCount; i++ {
		taskID := fmt.Sprintf("task-%d", i+1)
		snapshot := jobs.TaskSnapshot{
			TaskRevision: 1, ManifestSHA256: fmt.Sprintf("%064x", i+1),
			SourceSHA256: fmt.Sprintf("%064x", i+101), EvaluatorSHA256: fmt.Sprintf("%064x", i+201),
			ImageID: "sha256:agent-image", AgentConfigSHA256: "",
		}
		tasks[i] = jobs.DatasetTask{TaskID: taskID, Snapshot: snapshot}
		snapshot.AgentConfigSHA256 = fmt.Sprintf("%064x", i+301)
		prepared[i] = jobs.PreparedJob{Request: jobs.Request{SchemaVersion: 1, TaskID: taskID, Agent: "scripted", Script: "success"}, Snapshot: snapshot}
	}
	dataset, err := store.CreateDatasetVersion(ctx, admin.ID, project.ID, "dataset", "v1", tasks)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	return &managementFixture{store: store, db: store.db, dsn: dsn, admin: admin, project: project, dataset: dataset, tasks: tasks, prepared: prepared}
}

func (f *managementFixture) submit(t *testing.T, key string, count int) (jobs.Experiment, bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return f.store.SubmitExperiment(ctx, f.admin.ID, f.project.ID, f.dataset.ID, "experiment", key, f.prepared[:count], 1000)
}

func TestPostgresManagementQuotaIdempotencyAndCancelAccounting(t *testing.T) {
	f := newManagementFixture(t, 2, 2, 3)
	experiment, reused, err := f.submit(t, "same-key", 2)
	if err != nil || reused || len(experiment.Jobs) != 2 {
		t.Fatalf("first experiment: reused=%v jobs=%d err=%v", reused, len(experiment.Jobs), err)
	}
	replay, reused, err := f.submit(t, "same-key", 2)
	if err != nil || !reused || replay.ID != experiment.ID || len(replay.Jobs) != 2 {
		t.Fatalf("idempotent replay: reused=%v id=%s err=%v", reused, replay.ID, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err = f.store.SubmitExperiment(ctx, f.admin.ID, f.project.ID, f.dataset.ID, "different", "same-key", f.prepared[:2], 1000)
	if !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("same key with changed payload: got %v, want conflict", err)
	}
	_, _, err = f.submit(t, "over-quota", 1)
	if !errors.Is(err, jobs.ErrQuotaExceeded) {
		t.Fatalf("over quota: got %v, want quota exceeded", err)
	}
	for i := 0; i < 2; i++ {
		job, err := f.store.CancelProjectJob(ctx, f.admin.ID, f.project.ID, experiment.Jobs[0].ID)
		if err != nil || job.Status != jobs.StatusCanceled {
			t.Fatalf("cancel %d: status=%s err=%v", i, job.Status, err)
		}
	}
	usage, err := f.store.ProjectUsage(ctx, f.admin.ID, f.project.ID, "")
	if err != nil || usage.Reserved != 1 || usage.Used != 0 || usage.Released != 1 {
		t.Fatalf("usage after repeated cancel: %+v err=%v", usage, err)
	}
}

func TestPostgresManagementConcurrentSubmitsRespectDailyQuota(t *testing.T) {
	f := newManagementFixture(t, 1, 1, 1)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(index int) {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _, err := f.store.SubmitExperiment(ctx, f.admin.ID, f.project.ID, f.dataset.ID, "race", fmt.Sprintf("race-%d", index), f.prepared[:1], 1000)
			results <- err
		}(i)
	}
	close(start)
	successes, quotaErrors := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, jobs.ErrQuotaExceeded):
			quotaErrors++
		default:
			t.Fatalf("concurrent submission returned unexpected error: %v", err)
		}
	}
	if successes != 1 || quotaErrors != 1 {
		t.Fatalf("daily quota was not serialized: success=%d quota_errors=%d", successes, quotaErrors)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	usage, err := f.store.ProjectUsage(ctx, f.admin.ID, f.project.ID, "")
	if err != nil || usage.Reserved != 1 || usage.Used != 0 {
		t.Fatalf("usage after concurrent submissions: %+v err=%v", usage, err)
	}
}

func TestPostgresManagementConcurrentIdempotentSubmitsShareOneExperiment(t *testing.T) {
	f := newManagementFixture(t, 5, 2, 2)
	start := make(chan struct{})
	type submitResult struct {
		experiment jobs.Experiment
		reused     bool
		err        error
	}
	results := make(chan submitResult, 8)
	for i := 0; i < cap(results); i++ {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			experiment, reused, err := f.store.SubmitExperiment(ctx, f.admin.ID, f.project.ID, f.dataset.ID, "same-payload", "one-key", f.prepared[:2], 1000)
			results <- submitResult{experiment: experiment, reused: reused, err: err}
		}()
	}
	close(start)
	created, reused := 0, 0
	var canonical jobs.Experiment
	for i := 0; i < cap(results); i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent idempotent submit: %v", result.err)
		}
		if result.reused {
			reused++
		} else {
			created++
		}
		if canonical.ID == "" {
			canonical = result.experiment
		}
		if result.experiment.ID != canonical.ID || len(result.experiment.Jobs) != len(canonical.Jobs) {
			t.Fatalf("idempotent submit returned a second experiment: id=%s canonical=%s", result.experiment.ID, canonical.ID)
		}
		for j := range canonical.Jobs {
			if result.experiment.Jobs[j].ID != canonical.Jobs[j].ID {
				t.Fatalf("idempotent submit changed job identity at index %d", j)
			}
		}
	}
	if created != 1 || reused != 7 {
		t.Fatalf("idempotency outcomes: created=%d reused=%d", created, reused)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	usage, err := f.store.ProjectUsage(ctx, f.admin.ID, f.project.ID, "")
	if err != nil || usage.Reserved != 2 || usage.Used != 0 {
		t.Fatalf("usage after concurrent idempotency replay: %+v err=%v", usage, err)
	}
}

func TestPostgresManagementSubmitRollbackAndFinishOnce(t *testing.T) {
	f := newManagementFixture(t, 10, 2, 3)
	if _, err := f.db.ExecContext(context.Background(), `ALTER TABLE agenteval_jobs ADD CONSTRAINT management_test_fail_second CHECK (experiment_index IS NULL OR experiment_index <> 1)`); err != nil {
		t.Fatalf("install rollback constraint: %v", err)
	}
	_, _, err := f.submit(t, "rollback", 2)
	if err == nil {
		t.Fatal("expected the second inserted job to fail")
	}
	var experiments, managedJobs, reservations, reserved int
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM agenteval_experiments`).Scan(&experiments); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM agenteval_jobs WHERE project_id=$1`, f.project.ID).Scan(&managedJobs); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM agenteval_job_reservations r JOIN agenteval_jobs j ON j.id=r.job_id WHERE j.project_id=$1`, f.project.ID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(context.Background(), `SELECT COALESCE(sum(reserved),0) FROM agenteval_project_daily_usage WHERE project_id=$1`, f.project.ID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if experiments != 0 || managedJobs != 0 || reservations != 0 || reserved != 0 {
		t.Fatalf("partial experiment escaped rollback: experiments=%d jobs=%d reservations=%d reserved=%d", experiments, managedJobs, reservations, reserved)
	}
	if _, err := f.db.ExecContext(context.Background(), `ALTER TABLE agenteval_jobs DROP CONSTRAINT management_test_fail_second`); err != nil {
		t.Fatal(err)
	}
	experiment, reused, err := f.submit(t, "rollback", 2)
	if err != nil || reused || len(experiment.Jobs) != 2 {
		t.Fatalf("same key after rolled-back submit: reused=%v jobs=%d err=%v", reused, len(experiment.Jobs), err)
	}
	if _, reused, err := f.submit(t, "rollback", 2); err != nil || !reused {
		t.Fatalf("successful retry idempotency: reused=%v err=%v", reused, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claimed, err := f.store.Claim(ctx, "test-worker")
	if err != nil || claimed.ID != experiment.Jobs[0].ID {
		t.Fatalf("claim: job=%s err=%v", claimed.ID, err)
	}
	result, _ := json.Marshal(map[string]any{"status": "done"})
	first, err := f.store.Finish(ctx, claimed.ID, "test-worker", jobs.StatusSucceeded, result, "")
	if err != nil || !first {
		t.Fatalf("first finish: updated=%v err=%v", first, err)
	}
	second, err := f.store.Finish(ctx, claimed.ID, "test-worker", jobs.StatusSucceeded, result, "")
	if err != nil || second {
		t.Fatalf("repeated finish: updated=%v err=%v", second, err)
	}
	usage, err := f.store.ProjectUsage(ctx, f.admin.ID, f.project.ID, "")
	if err != nil || usage.Reserved != 1 || usage.Used != 1 || usage.Running != 0 {
		t.Fatalf("usage after finish: %+v err=%v", usage, err)
	}
	if _, err := f.store.CancelProjectJob(ctx, f.admin.ID, f.project.ID, experiment.Jobs[1].ID); err != nil {
		t.Fatalf("release queued sibling: %v", err)
	}
	usage, err = f.store.ProjectUsage(ctx, f.admin.ID, f.project.ID, "")
	if err != nil || usage.Reserved != 0 || usage.Used != 1 || usage.Released != 1 {
		t.Fatalf("usage after finish and queued cancel: %+v err=%v", usage, err)
	}
}

func TestPostgresManagementRecoverChargesRunningReservationOnce(t *testing.T) {
	f := newManagementFixture(t, 5, 1, 2)
	experiment, _, err := f.submit(t, "recover", 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	claimed, err := f.store.Claim(ctx, "crashed-worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(ctx, f.dsn, 4)
	if err != nil {
		t.Fatalf("reopen store after controller restart: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	f.store = restarted
	n, err := f.store.Recover(ctx)
	if err != nil || n != 1 {
		t.Fatalf("recover: changed=%d err=%v", n, err)
	}
	n, err = f.store.Recover(ctx)
	if err != nil || n != 0 {
		t.Fatalf("repeat recover: changed=%d err=%v", n, err)
	}
	runningJob, err := f.store.GetProjectJob(ctx, f.admin.ID, f.project.ID, claimed.ID)
	if err != nil || runningJob.Status != jobs.StatusInfraError || runningJob.ID != experiment.Jobs[0].ID && runningJob.ID != experiment.Jobs[1].ID {
		t.Fatalf("recovered running job: id=%s status=%s err=%v", runningJob.ID, runningJob.Status, err)
	}
	queuedID := experiment.Jobs[0].ID
	if queuedID == claimed.ID {
		queuedID = experiment.Jobs[1].ID
	}
	queuedJob, err := f.store.GetProjectJob(ctx, f.admin.ID, f.project.ID, queuedID)
	if err != nil || queuedJob.Status != jobs.StatusQueued {
		t.Fatalf("queued sibling after recovery: id=%s status=%s err=%v", queuedJob.ID, queuedJob.Status, err)
	}
	usage, err := f.store.ProjectUsage(ctx, f.admin.ID, f.project.ID, "")
	if err != nil || usage.Reserved != 1 || usage.Used != 1 || usage.Running != 0 {
		t.Fatalf("usage after recovery: %+v err=%v", usage, err)
	}
}

func TestPostgresManagementConcurrentClaimsRespectProjectLimit(t *testing.T) {
	f := newManagementFixture(t, 5, 1, 2)
	if _, _, err := f.submit(t, "concurrent-claims", 2); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan struct {
		job jobs.Job
		err error
	}, 2)
	for i := 0; i < 2; i++ {
		go func(index int) {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			job, err := f.store.Claim(ctx, fmt.Sprintf("racing-worker-%d", index))
			results <- struct {
				job jobs.Job
				err error
			}{job, err}
		}(i)
	}
	close(start)
	claimed := make(map[string]bool)
	noJob := 0
	for i := 0; i < 2; i++ {
		result := <-results
		if errors.Is(result.err, jobs.ErrNoJob) {
			noJob++
			continue
		}
		if result.err != nil {
			t.Fatalf("concurrent claim failed: %v", result.err)
		}
		claimed[result.job.ID] = true
	}
	if len(claimed) != 1 || noJob != 1 {
		t.Fatalf("project concurrent cap was not serialized: claimed=%d no_job=%d", len(claimed), noJob)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	usage, err := f.store.ProjectUsage(ctx, f.admin.ID, f.project.ID, "")
	if err != nil || usage.Running != 1 {
		t.Fatalf("running count after concurrent claims: %+v err=%v", usage, err)
	}
}

func TestPostgresManagementClaimHonorsPerProjectConcurrentLimit(t *testing.T) {
	f := newManagementFixture(t, 10, 1, 2)
	first, _, err := f.submit(t, "project-a", 2)
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.store.CreateProject(context.Background(), f.admin.ID, "other", 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	otherDataset, err := f.store.CreateDatasetVersion(context.Background(), f.admin.ID, other.ID, "dataset", "v1", f.tasks)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	otherExperiment, _, err := f.store.SubmitExperiment(ctx, f.admin.ID, other.ID, otherDataset.ID, "other", "project-b", f.prepared[:1], 1000)
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.store.Claim(ctx, "worker-a")
	if err != nil || a.ExperimentID != first.ID || a.ProjectID != f.project.ID {
		t.Fatalf("first claim: project=%s experiment=%s err=%v", a.ProjectID, a.ExperimentID, err)
	}
	b, err := f.store.Claim(ctx, "worker-b")
	if err != nil || b.ID != otherExperiment.Jobs[0].ID {
		t.Fatalf("claim should skip project at limit and advance: id=%s err=%v", b.ID, err)
	}
	if b.ProjectID != other.ID {
		t.Fatalf("second claim came from project %s, want %s", b.ProjectID, other.ID)
	}
}

func TestPostgresManagementCannotLowerLimitsBelowCurrentUsage(t *testing.T) {
	f := newManagementFixture(t, 10, 2, 2)
	if _, _, err := f.submit(t, "limits", 2); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.store.UpdateProjectLimits(ctx, f.admin.ID, f.project.ID, 1, 2); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("lower daily quota below reservations: got %v", err)
	}
	if _, err := f.store.Claim(ctx, "limit-worker-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Claim(ctx, "limit-worker-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpdateProjectLimits(ctx, f.admin.ID, f.project.ID, 10, 1); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("lower concurrent quota below running jobs: got %v", err)
	}
	if _, err := f.store.UpdateProjectLimits(ctx, f.admin.ID, f.project.ID, 10, 2); err != nil {
		t.Fatalf("set limits at current usage: %v", err)
	}
}

func TestPostgresManagementMembershipAndLastOwner(t *testing.T) {
	f := newManagementFixture(t, 10, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	user, err := f.store.CreateUser(ctx, f.admin.ID, "member-user", "placeholder-hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.GetProject(ctx, user.ID, f.project.ID); !errors.Is(err, jobs.ErrForbidden) {
		t.Fatalf("nonmember project read: got %v", err)
	}
	if _, err := f.store.GetProject(ctx, f.admin.ID, "ffffffffffffffffffffffff"); !errors.Is(err, jobs.ErrForbidden) {
		t.Fatalf("site admin must not bypass project membership: got %v", err)
	}
	if err := f.store.SetMember(ctx, f.admin.ID, f.project.ID, user.Username, "reader"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateDatasetVersion(ctx, user.ID, f.project.ID, "reader-write", "v1", f.tasks); !errors.Is(err, jobs.ErrForbidden) {
		t.Fatalf("reader dataset write: got %v", err)
	}
	if err := f.store.SetMember(ctx, f.admin.ID, f.project.ID, user.Username, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RemoveMember(ctx, f.admin.ID, f.project.ID, user.ID); err != nil {
		t.Fatalf("remove secondary owner: %v", err)
	}
	if err := f.store.RemoveMember(ctx, f.admin.ID, f.project.ID, f.admin.ID); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("remove last owner: got %v", err)
	}
	if err := f.store.SetMember(ctx, f.admin.ID, f.project.ID, f.admin.Username, "executor"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("demote last owner: got %v", err)
	}
}

func TestPostgresManagementDailyUsageUsesReservationDay(t *testing.T) {
	f := newManagementFixture(t, 5, 1, 1)
	experiment, _, err := f.submit(t, "cross-day", 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var day string
	if err := f.db.QueryRowContext(ctx, `SELECT to_char((clock_timestamp() AT TIME ZONE 'UTC')::date,'YYYY-MM-DD')`).Scan(&day); err != nil {
		t.Fatal(err)
	}
	var yesterday string
	if err := f.db.QueryRowContext(ctx, `SELECT to_char(((clock_timestamp() AT TIME ZONE 'UTC')::date - 1),'YYYY-MM-DD')`).Scan(&yesterday); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO agenteval_project_daily_usage(project_id,usage_day,reserved) VALUES($1,$2::date,1)`, f.project.ID, yesterday); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE agenteval_job_reservations SET usage_day=$2::date WHERE job_id=$1`, experiment.Jobs[0].ID, yesterday); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE agenteval_project_daily_usage SET reserved=reserved-1 WHERE project_id=$1 AND usage_day=$2::date`, f.project.ID, day); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.store.Claim(ctx, "cross-day-worker")
	if err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"status": "done"})
	if ok, err := f.store.Finish(ctx, claimed.ID, "cross-day-worker", jobs.StatusSucceeded, result, ""); err != nil || !ok {
		t.Fatalf("finish: ok=%v err=%v", ok, err)
	}
	usage, err := f.store.ProjectUsage(ctx, f.admin.ID, f.project.ID, yesterday)
	if err != nil || usage.Reserved != 0 || usage.Used != 1 {
		t.Fatalf("prior-day usage after finish: %+v err=%v", usage, err)
	}
}
