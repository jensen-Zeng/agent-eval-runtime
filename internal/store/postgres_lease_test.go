package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

// Integration tests use an isolated schema in a dedicated PostgreSQL test
// database. They are opt-in so ordinary builds never require a database.
func integrationStore(tb testing.TB) (*Postgres, *sql.DB, string) {
	tb.Helper()
	dsn := os.Getenv("AGENTEVAL_TEST_DATABASE_URL")
	if dsn == "" {
		tb.Skip("set AGENTEVAL_TEST_DATABASE_URL to a dedicated PostgreSQL test database")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		tb.Fatalf("AGENTEVAL_TEST_DATABASE_URL must be a postgres:// URL")
	}
	base, err := sql.Open("pgx", dsn)
	if err != nil {
		tb.Fatalf("open test database: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := base.PingContext(ctx); err != nil {
		_ = base.Close()
		tb.Fatalf("ping test database: %v", err)
	}
	schemaID, err := randomID()
	if err != nil {
		tb.Fatalf("generate schema name: %v", err)
	}
	schema := "agenteval_test_" + schemaID
	if _, err := base.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		_ = base.Close()
		tb.Fatalf("create isolated schema: %v", err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	store, err := New(ctx, u.String(), 4)
	if err != nil {
		_, _ = base.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		_ = base.Close()
		tb.Fatalf("open test store: %v", err)
	}
	tb.Cleanup(func() {
		_ = store.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = base.ExecContext(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`)
		_ = base.Close()
	})
	return store, base, u.String()
}

func submitIntegrationJob(t *testing.T, store *Postgres) jobs.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := jobs.Request{SchemaVersion: 1, TaskID: "lease-test", Agent: "scripted", Script: "success"}
	snapshot := jobs.TaskSnapshot{
		TaskRevision: 1, ManifestSHA256: fmt.Sprintf("%064x", 1),
		SourceSHA256: fmt.Sprintf("%064x", 2), EvaluatorSHA256: fmt.Sprintf("%064x", 3),
		ImageID: "sha256:fixture", AgentConfigSHA256: fmt.Sprintf("%064x", 4),
	}
	job, _, err := store.Submit(ctx, request, snapshot, "", fmt.Sprintf("%064x", 5), 10)
	if err != nil {
		t.Fatalf("submit test job: %v", err)
	}
	return job
}

func TestPostgresFencingRejectsStaleOwner(t *testing.T) {
	store, _, _ := integrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job := submitIntegrationJob(t, store)
	claimed, err := store.Claim(ctx, "attempt-owner")
	if err != nil || claimed.ID != job.ID {
		t.Fatalf("claim test job: job=%s err=%v", claimed.ID, err)
	}
	newOwner := "0123456789abcdef01234567"
	if _, err := store.db.ExecContext(ctx, `UPDATE agenteval_lease SET lease_owner = $1,
		epoch = epoch + 1, lease_until = clock_timestamp() + interval '30 seconds' WHERE singleton = 1`, newOwner); err != nil {
		t.Fatalf("simulate lease takeover: %v", err)
	}
	ok, err := store.Finish(ctx, job.ID, "attempt-owner", jobs.StatusSucceeded, json.RawMessage(`{"status":"SUCCEEDED"}`), "")
	if ok || !errors.Is(err, jobs.ErrLeadershipLost) {
		t.Fatalf("stale finish = (%v, %v), want rejected by fencing", ok, err)
	}
}

func TestPostgresExpiredLeaseCannotBeRenewed(t *testing.T) {
	store, _, _ := integrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := store.db.ExecContext(ctx, `UPDATE agenteval_lease SET lease_until = clock_timestamp() - interval '1 second' WHERE singleton = 1`); err != nil {
		t.Fatalf("expire test lease: %v", err)
	}
	if err := store.Renew(ctx); !errors.Is(err, jobs.ErrLeadershipLost) {
		t.Fatalf("renew expired lease = %v, want leadership lost", err)
	}
	var stillExpired bool
	if err := store.db.QueryRowContext(ctx, `SELECT lease_until <= clock_timestamp() FROM agenteval_lease WHERE singleton = 1`).Scan(&stillExpired); err != nil {
		t.Fatalf("read lease after rejected renewal: %v", err)
	}
	if !stillExpired {
		t.Fatal("rejected heartbeat resurrected the expired lease")
	}
	if err := store.Health(ctx); !errors.Is(err, jobs.ErrLeadershipLost) {
		t.Fatalf("health for expired lease = %v, want leadership lost", err)
	}
}

func TestPostgresCommitRejectsLeaseExpiredDuringTransaction(t *testing.T) {
	store, _, _ := integrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job := submitIntegrationJob(t, store)
	tx, err := store.beginFencedTx(ctx)
	if err != nil {
		t.Fatalf("begin fenced transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE agenteval_jobs SET status = $2 WHERE id = $1`, job.ID, jobs.StatusRunning); err != nil {
		t.Fatalf("stage business mutation: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agenteval_lease SET lease_until = clock_timestamp() + interval '1 millisecond' WHERE singleton = 1`); err != nil {
		t.Fatalf("shorten lease in transaction: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_sleep(0.02)`); err != nil {
		t.Fatalf("wait for lease to expire: %v", err)
	}
	if err := store.commitFencedTx(ctx, tx); !errors.Is(err, jobs.ErrLeadershipLost) {
		t.Fatalf("commit after lease expiry = %v, want leadership lost", err)
	}
	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("read job after rejected commit: %v", err)
	}
	if got.Status != jobs.StatusQueued {
		t.Fatalf("stale transaction committed status %q", got.Status)
	}
}

func TestPostgresStartupWaitsForExpiredLease(t *testing.T) {
	store, _, storeDSN := integrationStore(t)
	oldEpoch := store.Epoch()
	newOwner := "abcdefabcdefabcdefabcdef"
	if _, err := store.db.Exec(`UPDATE agenteval_lease SET lease_owner = $1, epoch = epoch + 1,
		lease_until = clock_timestamp() + interval '900 milliseconds' WHERE singleton = 1`, newOwner); err != nil {
		t.Fatalf("simulate crashed owner lease: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("release old controller lock: %v", err)
	}
	// The shared integration helper owns cleanup, so prevent its double-close
	// from masking the behavior under test; lease release itself is idempotent.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	store, err := New(ctx, storeDSN, 4)
	if err != nil {
		t.Fatalf("new controller did not wait for lease expiry: %v", err)
	}
	defer store.Close()
	if store.Epoch() <= oldEpoch+1 {
		t.Fatalf("epoch did not advance on takeover: old=%d new=%d", oldEpoch, store.Epoch())
	}
}

func BenchmarkPostgresBoundedList(b *testing.B) {
	store, _, _ := integrationStore(b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const rows = 1000
	for i := 1; i <= rows; i++ {
		id := fmt.Sprintf("%024x", i)
		if _, err := store.db.ExecContext(ctx, `INSERT INTO agenteval_jobs
			(id, status, request, task_snapshot, request_sha256)
			VALUES ($1, $2, '{"schema_version":1,"task_id":"bench","agent":"scripted"}'::jsonb,
			'{"task_revision":1}'::jsonb, $3)`, id, jobs.StatusQueued, fmt.Sprintf("%064x", i)); err != nil {
			b.Fatalf("seed list benchmark: %v", err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.List(ctx, 50, ""); err != nil {
			b.Fatalf("bounded list: %v", err)
		}
	}
}
