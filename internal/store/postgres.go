package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"github.com/jensen-Zeng/agent-eval-runtime/migrations"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const advisoryLockKey int64 = 0x4147454e54455632

// LeaseTTL is deliberately much longer than the scheduler's two-second
// heartbeat interval. PostgreSQL's clock is authoritative for lease decisions.
const LeaseTTL = 30 * time.Second

const jobColumns = `id, status, request, task_snapshot, idempotency_key, request_sha256,
created_at, updated_at, started_at, finished_at, cancel_requested, owner_token,
run_id, attempt_id, result, reason`

type Postgres struct {
	db      *sql.DB
	leader  *sql.Conn
	ownerID string
	process string
	epoch   int64
	mu      sync.Mutex
	closed  bool
}

type scanner interface {
	Scan(...any) error
}

// New opens PostgreSQL, takes the single-service advisory lock, applies embedded
// migrations, and creates the persistent owner identity. Call Recover only after
// owner-scoped resource cleanup has completed.
func New(ctx context.Context, dsn string, maxConnections int) (*Postgres, error) {
	if strings.TrimSpace(dsn) == "" || maxConnections < 2 {
		return nil, jobs.ErrInvalidRequest
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres open: %w", jobs.ErrUnavailable)
	}
	db.SetMaxOpenConns(maxConnections)
	db.SetMaxIdleConns(maxConnections)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres ping: %w", jobs.ErrUnavailable)
	}
	leader, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres leader connection: %w", jobs.ErrUnavailable)
	}
	var acquired bool
	if err := leader.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, advisoryLockKey).Scan(&acquired); err != nil {
		_ = leader.Close()
		_ = db.Close()
		return nil, fmt.Errorf("postgres leadership check: %w", jobs.ErrUnavailable)
	}
	if !acquired {
		_ = leader.Close()
		_ = db.Close()
		return nil, jobs.ErrAlreadyRunning
	}
	p := &Postgres{db: db, leader: leader}
	if err := p.migrate(ctx); err != nil {
		_ = p.Close()
		return nil, err
	}
	if err := p.loadOwnerID(ctx); err != nil {
		_ = p.Close()
		return nil, err
	}
	if p.process, err = randomID(); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("postgres process identity: %w", jobs.ErrUnavailable)
	}
	if err := p.acquireLeaseWithWait(ctx); err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) acquireLeaseWithWait(ctx context.Context) error {
	// Leave room for the next poll at the TTL boundary to observe expiry.
	waitCtx, cancel := context.WithTimeout(ctx, LeaseTTL+time.Second)
	defer cancel()
	for {
		err := p.acquireLease(waitCtx)
		if !errors.Is(err, jobs.ErrAlreadyRunning) {
			return err
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-timer.C:
		case <-waitCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return jobs.ErrAlreadyRunning
		}
	}
}

func (p *Postgres) migrate(ctx context.Context) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres migration begin: %w", jobs.ErrUnavailable)
	}
	defer func() { _ = tx.Rollback() }()
	for _, migration := range []string{migrations.JobsSQL, migrations.LeaseSQL} {
		for _, statement := range strings.Split(migration, ";") {
			statement = strings.TrimSpace(statement)
			if statement == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("postgres migration apply (%s): %v: %w", migrationName(migration), err, jobs.ErrUnavailable)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres migration commit: %w", jobs.ErrUnavailable)
	}
	return nil
}

func migrationName(sql string) string {
	if strings.Contains(sql, "agenteval_lease") {
		return "lease"
	}
	return "jobs"
}

// acquireLease advances the fencing epoch only after the previous lease has
// expired according to the database clock. A new controller must retry after
// ErrAlreadyRunning if the old lease has not expired yet.
func (p *Postgres) acquireLease(ctx context.Context) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin lease transaction: %v: %w", err, jobs.ErrUnavailable)
	}
	defer func() { _ = tx.Rollback() }()
	var currentOwner sql.NullString
	var currentEpoch int64
	var leaseUntil time.Time
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner, epoch, lease_until FROM agenteval_lease WHERE singleton = 1 FOR UPDATE`).Scan(&currentOwner, &currentEpoch, &leaseUntil); err != nil {
		return fmt.Errorf("read lease row: %v: %w", err, jobs.ErrUnavailable)
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read database clock: %v: %w", err, jobs.ErrUnavailable)
	}
	if currentOwner.Valid && leaseUntil.After(now) {
		return jobs.ErrAlreadyRunning
	}
	if err := tx.QueryRowContext(ctx, `UPDATE agenteval_lease SET lease_owner = $1, epoch = epoch + 1,
		lease_until = clock_timestamp() + ($2 * interval '1 millisecond')
		WHERE singleton = 1 RETURNING epoch`, p.process, LeaseTTL.Milliseconds()).Scan(&p.epoch); err != nil {
		return fmt.Errorf("write lease row: %v: %w", err, jobs.ErrUnavailable)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit lease acquisition: %v: %w", err, jobs.ErrUnavailable)
	}
	return nil
}

func (p *Postgres) loadOwnerID(ctx context.Context) error {
	owner, err := randomID()
	if err != nil {
		return fmt.Errorf("postgres owner identity: %w", jobs.ErrUnavailable)
	}
	if _, err := p.db.ExecContext(ctx, `INSERT INTO agenteval_metadata (singleton, owner_id) VALUES (1, $1) ON CONFLICT (singleton) DO NOTHING`, owner); err != nil {
		return fmt.Errorf("postgres owner identity: %w", jobs.ErrUnavailable)
	}
	if err := p.db.QueryRowContext(ctx, `SELECT owner_id FROM agenteval_metadata WHERE singleton = 1`).Scan(&p.ownerID); err != nil {
		return fmt.Errorf("postgres owner identity: %w", jobs.ErrUnavailable)
	}
	return nil
}

func (p *Postgres) OwnerID() string { return p.ownerID }

// Epoch identifies this controller's lease generation. It increases whenever
// an expired lease is acquired and is suitable as a fencing value in artifacts.
func (p *Postgres) Epoch() int64 { return p.epoch }

// beginFencedTx serializes a business mutation against lease takeover while
// allowing unrelated mutations in the current epoch to proceed concurrently.
func (p *Postgres) beginFencedTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, jobs.ErrUnavailable
	}
	var process sql.NullString
	var epoch int64
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT lease_owner, epoch, lease_until > clock_timestamp()
		FROM agenteval_lease WHERE singleton = 1 FOR SHARE`).Scan(&process, &epoch, &valid); err != nil {
		_ = tx.Rollback()
		return nil, jobs.ErrUnavailable
	}
	if !process.Valid || process.String != p.process || epoch != p.epoch || !valid {
		_ = tx.Rollback()
		return nil, jobs.ErrLeadershipLost
	}
	return tx, nil
}

// commitFencedTx rechecks the DB-clock lease immediately before committing.
// The lease row share lock is held throughout the transaction, so a new epoch
// cannot be installed between this check and commit.
func (p *Postgres) commitFencedTx(ctx context.Context, tx *sql.Tx) error {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT lease_owner = $1 AND epoch = $2 AND lease_until > clock_timestamp()
		FROM agenteval_lease WHERE singleton = 1`, p.process, p.epoch).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !valid) {
		_ = tx.Rollback()
		return jobs.ErrLeadershipLost
	}
	if err != nil {
		_ = tx.Rollback()
		return jobs.ErrUnavailable
	}
	if err := tx.Commit(); err != nil {
		return jobs.ErrUnavailable
	}
	return nil
}

// renewLease extends only a still-live lease owned by this process and epoch.
// The SQL predicate is rechecked after any row-lock wait, so an expired lease
// can never be resurrected by a delayed heartbeat.
func (p *Postgres) renewLease(ctx context.Context) error {
	if err := p.ensureLeader(ctx); err != nil {
		return err
	}
	var renewed bool
	err := p.db.QueryRowContext(ctx, `UPDATE agenteval_lease SET
		lease_until = clock_timestamp() + ($1 * interval '1 millisecond')
		WHERE singleton = 1 AND lease_owner = $2 AND epoch = $3
		AND lease_until > clock_timestamp() RETURNING true`, LeaseTTL.Milliseconds(), p.process, p.epoch).Scan(&renewed)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.ErrLeadershipLost
	}
	if err != nil || !renewed {
		return jobs.ErrUnavailable
	}
	return nil
}

func (p *Postgres) ensureLeader(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.leader == nil {
		return jobs.ErrLeadershipLost
	}
	var held bool
	err := p.leader.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted AND objsubid = 1
		  AND classid = (($1::bigint >> 32) & 4294967295)::oid
		  AND objid = ($1::bigint & 4294967295)::oid
	)`, advisoryLockKey).Scan(&held)
	if err != nil || !held {
		return jobs.ErrLeadershipLost
	}
	return nil
}

func (p *Postgres) Health(ctx context.Context) error {
	if err := p.ensureLeader(ctx); err != nil {
		return err
	}
	var current bool
	err := p.db.QueryRowContext(ctx, `SELECT lease_owner = $1 AND epoch = $2 AND lease_until > clock_timestamp()
		FROM agenteval_lease WHERE singleton = 1`, p.process, p.epoch).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !current) {
		return jobs.ErrLeadershipLost
	}
	if err != nil {
		return jobs.ErrUnavailable
	}
	if err := p.db.PingContext(ctx); err != nil {
		return jobs.ErrUnavailable
	}
	return nil
}

// Renew extends this controller's lease. Schedulers should call it only from
// their bounded heartbeat loop, rather than from request readiness checks.
func (p *Postgres) Renew(ctx context.Context) error {
	if err := p.renewLease(ctx); err != nil {
		return err
	}
	return nil
}

func (p *Postgres) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var closeErr error
	if p.db != nil && p.process != "" && p.epoch > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := p.db.ExecContext(ctx, `UPDATE agenteval_lease SET lease_owner = NULL, lease_until = clock_timestamp()
			WHERE singleton = 1 AND lease_owner = $1 AND epoch = $2 AND lease_until > clock_timestamp()`, p.process, p.epoch)
		cancel()
		if err != nil {
			closeErr = jobs.ErrUnavailable
		}
	}
	if p.leader != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		var unlocked bool
		err := p.leader.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey).Scan(&unlocked)
		cancel()
		if err != nil || !unlocked {
			closeErr = jobs.ErrLeadershipLost
		}
		if err := p.leader.Close(); err != nil && closeErr == nil {
			closeErr = jobs.ErrUnavailable
		}
	}
	if p.db != nil {
		if err := p.db.Close(); err != nil && closeErr == nil {
			closeErr = jobs.ErrUnavailable
		}
	}
	return closeErr
}

func (p *Postgres) Submit(ctx context.Context, request jobs.Request, snapshot jobs.TaskSnapshot, key, digest string, capacity int) (jobs.Job, bool, error) {
	if err := validateSubmission(request, snapshot, key, digest, capacity); err != nil {
		return jobs.Job{}, false, err
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return jobs.Job{}, false, jobs.ErrInvalidRequest
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return jobs.Job{}, false, jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return jobs.Job{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize idempotency and queue-capacity checks across API requests.
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM agenteval_metadata WHERE singleton = 1 FOR UPDATE`).Scan(&owner); err != nil {
		return jobs.Job{}, false, jobs.ErrUnavailable
	}
	if key != "" {
		existing, findErr := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM agenteval_jobs WHERE idempotency_key = $1`, key))
		if findErr == nil {
			if existing.RequestSHA256 != digest {
				return jobs.Job{}, false, jobs.ErrConflict
			}
			if err := p.commitFencedTx(ctx, tx); err != nil {
				return jobs.Job{}, false, err
			}
			return existing, true, nil
		}
		if !errors.Is(findErr, sql.ErrNoRows) {
			return jobs.Job{}, false, jobs.ErrUnavailable
		}
	}
	var queued int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agenteval_jobs WHERE status = $1`, jobs.StatusQueued).Scan(&queued); err != nil {
		return jobs.Job{}, false, jobs.ErrUnavailable
	}
	if capacity > 0 && queued >= int64(capacity) {
		return jobs.Job{}, false, jobs.ErrQueueFull
	}
	id, err := randomID()
	if err != nil {
		return jobs.Job{}, false, jobs.ErrUnavailable
	}
	var idem any
	if key != "" {
		idem = key
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agenteval_jobs
		(id, status, request, task_snapshot, idempotency_key, request_sha256)
		VALUES ($1, $2, $3::jsonb, $4::jsonb, $5, $6)`, id, jobs.StatusQueued, requestJSON, snapshotJSON, idem, digest)
	if err != nil {
		return jobs.Job{}, false, jobs.ErrUnavailable
	}
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM agenteval_jobs WHERE id = $1`, id))
	if err != nil {
		return jobs.Job{}, false, jobs.ErrUnavailable
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return jobs.Job{}, false, err
	}
	return job, false, nil
}

func validateSubmission(request jobs.Request, snapshot jobs.TaskSnapshot, key, digest string, capacity int) error {
	if request.SchemaVersion != 1 || request.TaskID == "" || (request.Agent != "scripted" && request.Agent != "llm") {
		return jobs.ErrInvalidRequest
	}
	if request.Agent == "scripted" && request.Script == "" {
		return jobs.ErrInvalidRequest
	}
	if snapshot.TaskRevision < 1 || snapshot.ImageID == "" || !isHex(snapshot.ManifestSHA256, 32) || !isHex(snapshot.SourceSHA256, 32) || !isHex(snapshot.EvaluatorSHA256, 32) || !isHex(snapshot.AgentConfigSHA256, 32) || !isHex(digest, 32) {
		return jobs.ErrInvalidRequest
	}
	if !jobs.ValidIdempotencyKey(key) || capacity < 0 {
		return jobs.ErrInvalidRequest
	}
	return nil
}

func isHex(value string, bytes int) bool {
	if len(value) != bytes*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (p *Postgres) Claim(ctx context.Context, ownerToken string) (jobs.Job, error) {
	if ownerToken == "" || len(ownerToken) > 255 {
		return jobs.Job{}, jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return jobs.Job{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM agenteval_jobs
		WHERE status = $1 AND NOT cancel_requested
		ORDER BY created_at ASC, id ASC
		LIMIT 1 FOR UPDATE SKIP LOCKED`, jobs.StatusQueued).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, jobs.ErrNoJob
	}
	if err != nil {
		return jobs.Job{}, jobs.ErrUnavailable
	}
	_, err = tx.ExecContext(ctx, `UPDATE agenteval_jobs SET status = $2, owner_token = $3,
		started_at = COALESCE(started_at, now()), updated_at = now()
		WHERE id = $1 AND status = $4 AND NOT cancel_requested`, id, jobs.StatusRunning, ownerToken, jobs.StatusQueued)
	if err != nil {
		return jobs.Job{}, jobs.ErrUnavailable
	}
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM agenteval_jobs WHERE id = $1`, id))
	if err != nil {
		return jobs.Job{}, jobs.ErrUnavailable
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return jobs.Job{}, err
	}
	return job, nil
}

func (p *Postgres) Get(ctx context.Context, id string) (jobs.Job, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return jobs.Job{}, err
	}
	if !validID(id) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	job, err := scanJob(p.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM agenteval_jobs WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, jobs.ErrUnavailable
	}
	return job, nil
}

func (p *Postgres) List(ctx context.Context, limit int, status string) ([]jobs.Job, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 500 || (status != "" && !validStatus(status)) {
		return nil, jobs.ErrInvalidRequest
	}
	query := `SELECT ` + jobColumns + ` FROM agenteval_jobs ORDER BY created_at DESC, id DESC LIMIT $1`
	args := []any{limit}
	if status != "" {
		query = `SELECT ` + jobColumns + ` FROM agenteval_jobs WHERE status = $1 ORDER BY created_at DESC, id DESC LIMIT $2`
		args = []any{status, limit}
	}
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, jobs.ErrUnavailable
	}
	defer rows.Close()
	result := make([]jobs.Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, jobs.ErrUnavailable
		}
		result = append(result, job)
	}
	if err := rows.Err(); err != nil {
		return nil, jobs.ErrUnavailable
	}
	return result, nil
}

func (p *Postgres) SetAttempt(ctx context.Context, id, ownerToken, runID, attemptID string) (bool, error) {
	if !validID(id) || ownerToken == "" || runID == "" || attemptID == "" {
		return false, jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE agenteval_jobs SET run_id = $3, attempt_id = $4, updated_at = clock_timestamp()
		WHERE id = $1 AND status = $5 AND owner_token = $2 AND NOT cancel_requested`, id, ownerToken, runID, attemptID, jobs.StatusRunning)
	if err != nil {
		return false, jobs.ErrUnavailable
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, jobs.ErrUnavailable
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return false, err
	}
	return n == 1, nil
}

func (p *Postgres) Finish(ctx context.Context, id, ownerToken, status string, resultJSON json.RawMessage, reason string) (bool, error) {
	if !validID(id) || ownerToken == "" || !validTerminalStatus(status) || len(resultJSON) == 0 || !json.Valid(resultJSON) {
		return false, jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE agenteval_jobs SET
		status = CASE WHEN cancel_requested THEN $6 ELSE $3 END,
		result = $4::jsonb,
		reason = CASE WHEN cancel_requested THEN 'canceled' ELSE $5 END,
		finished_at = clock_timestamp(), updated_at = clock_timestamp(), owner_token = NULL
		WHERE id = $1 AND status = $7 AND owner_token = $2`, id, ownerToken, status, []byte(resultJSON), reason, jobs.StatusCanceled, jobs.StatusRunning)
	if err != nil {
		return false, jobs.ErrUnavailable
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, jobs.ErrUnavailable
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return false, err
	}
	return n == 1, nil
}

func validTerminalStatus(status string) bool {
	switch status {
	case jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusTimedOut, jobs.StatusCanceled, jobs.StatusInfraError:
		return true
	default:
		return false
	}
}

func validStatus(status string) bool {
	return status == jobs.StatusQueued || status == jobs.StatusRunning || validTerminalStatus(status)
}

func (p *Postgres) Cancel(ctx context.Context, id string) (jobs.Job, error) {
	if !validID(id) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return jobs.Job{}, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE agenteval_jobs SET
		cancel_requested = true,
		status = CASE WHEN status = $2 THEN $3 ELSE status END,
		reason = CASE WHEN status = $2 THEN 'canceled' ELSE reason END,
		finished_at = CASE WHEN status = $2 THEN clock_timestamp() ELSE finished_at END,
		owner_token = CASE WHEN status = $2 THEN NULL ELSE owner_token END,
		updated_at = clock_timestamp()
		WHERE id = $1 AND status IN ($2, $4)`, id, jobs.StatusQueued, jobs.StatusCanceled, jobs.StatusRunning)
	if err != nil {
		return jobs.Job{}, jobs.ErrUnavailable
	}
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM agenteval_jobs WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, jobs.ErrUnavailable
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return jobs.Job{}, err
	}
	return job, nil
}

func (p *Postgres) Recover(ctx context.Context) (int, error) {
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE agenteval_jobs SET
		status = CASE WHEN cancel_requested THEN $1 ELSE $2 END,
		reason = CASE WHEN cancel_requested THEN 'canceled' ELSE 'interrupted' END,
		finished_at = clock_timestamp(), updated_at = clock_timestamp(), owner_token = NULL
		WHERE status = $3`, jobs.StatusCanceled, jobs.StatusInfraError, jobs.StatusRunning)
	if err != nil {
		return 0, jobs.ErrUnavailable
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, jobs.ErrUnavailable
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return 0, err
	}
	return int(n), nil
}

func (p *Postgres) Summary(ctx context.Context) (jobs.Summary, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return jobs.Summary{}, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT status, count(*) FROM agenteval_jobs GROUP BY status`)
	if err != nil {
		return jobs.Summary{}, jobs.ErrUnavailable
	}
	defer rows.Close()
	byStatus := map[string]int64{
		jobs.StatusQueued: 0, jobs.StatusRunning: 0, jobs.StatusSucceeded: 0,
		jobs.StatusFailed: 0, jobs.StatusTimedOut: 0, jobs.StatusCanceled: 0,
		jobs.StatusInfraError: 0,
	}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return jobs.Summary{}, jobs.ErrUnavailable
		}
		byStatus[status] = count
	}
	if err := rows.Err(); err != nil {
		return jobs.Summary{}, jobs.ErrUnavailable
	}
	return jobs.Summary{Queued: byStatus[jobs.StatusQueued], Running: byStatus[jobs.StatusRunning], ByStatus: byStatus}, nil
}

func scanJob(row scanner) (jobs.Job, error) {
	var job jobs.Job
	var requestJSON, snapshotJSON, resultJSON []byte
	var key, ownerToken, runID, attemptID, reason sql.NullString
	var started, finished sql.NullTime
	err := row.Scan(&job.ID, &job.Status, &requestJSON, &snapshotJSON, &key, &job.RequestSHA256,
		&job.CreatedAt, &job.UpdatedAt, &started, &finished, &job.CancelRequested, &ownerToken,
		&runID, &attemptID, &resultJSON, &reason)
	if err != nil {
		return jobs.Job{}, err
	}
	if err := json.Unmarshal(requestJSON, &job.Request); err != nil {
		return jobs.Job{}, fmt.Errorf("stored request is invalid")
	}
	if err := json.Unmarshal(snapshotJSON, &job.TaskSnapshot); err != nil {
		return jobs.Job{}, fmt.Errorf("stored task snapshot is invalid")
	}
	job.IdempotencyKey = key.String
	job.OwnerToken = ownerToken.String
	job.RunID = runID.String
	job.AttemptID = attemptID.String
	job.Reason = reason.String
	if started.Valid {
		t := started.Time.UTC()
		job.StartedAt = &t
	}
	if finished.Valid {
		t := finished.Time.UTC()
		job.FinishedAt = &t
	}
	if len(resultJSON) > 0 {
		job.Result = append(json.RawMessage(nil), resultJSON...)
	}
	job.CreatedAt = job.CreatedAt.UTC()
	job.UpdatedAt = job.UpdatedAt.UTC()
	return job, nil
}

func randomID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func validID(id string) bool { return len(id) == 24 && isHex(id, 12) }
