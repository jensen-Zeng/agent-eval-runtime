package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

const maxDatasetTasks = 200
const maxExperimentJobs = 500
const maxProjectDailyLimit = 1_000_000
const maxProjectConcurrentLimit = 1_000

func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func validUsername(username string) bool {
	if username != strings.ToLower(strings.TrimSpace(username)) || len(username) < 3 || len(username) > 64 {
		return false
	}
	for _, r := range username {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
}

func validName(value string, max int) bool {
	if strings.TrimSpace(value) != value || len(value) == 0 || len(value) > max*4 || len([]rune(value)) > max {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validRole(role string) bool { return role == "owner" || role == "executor" || role == "reader" }

func (p *Postgres) HasUsers(ctx context.Context) (bool, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return false, err
	}
	var found bool
	err := p.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agenteval_users)`).Scan(&found)
	if err != nil {
		return false, jobs.ErrUnavailable
	}
	return found, nil
}

func (p *Postgres) Bootstrap(ctx context.Context, username, passwordHash string) (jobs.User, error) {
	if !validUsername(username) || passwordHash == "" || len(passwordHash) > 512 {
		return jobs.User{}, jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return jobs.User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockMetadata(ctx, tx); err != nil {
		return jobs.User{}, err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agenteval_users)`).Scan(&exists); err != nil {
		return jobs.User{}, jobs.ErrUnavailable
	}
	if exists {
		return jobs.User{}, jobs.ErrConflict
	}
	user, err := insertUser(ctx, tx, username, passwordHash, true)
	if err != nil {
		if uniqueViolation(err) {
			return jobs.User{}, jobs.ErrConflict
		}
		return jobs.User{}, err
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return jobs.User{}, err
	}
	return user, nil
}

func (p *Postgres) CreateUser(ctx context.Context, actorID, username, passwordHash string) (jobs.User, error) {
	if !validUsername(username) || passwordHash == "" || len(passwordHash) > 512 {
		return jobs.User{}, jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return jobs.User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireAdmin(ctx, tx, actorID); err != nil {
		return jobs.User{}, err
	}
	user, err := insertUser(ctx, tx, username, passwordHash, false)
	if err != nil {
		if uniqueViolation(err) {
			return jobs.User{}, jobs.ErrConflict
		}
		return jobs.User{}, err
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return jobs.User{}, err
	}
	return user, nil
}

func insertUser(ctx context.Context, tx *sql.Tx, username, passwordHash string, admin bool) (jobs.User, error) {
	id, err := randomID()
	if err != nil {
		return jobs.User{}, jobs.ErrUnavailable
	}
	var user jobs.User
	err = tx.QueryRowContext(ctx, `INSERT INTO agenteval_users (id, username, password_hash, is_admin)
		VALUES ($1,$2,$3,$4) RETURNING id, username, is_admin, created_at`, id, username, passwordHash, admin).
		Scan(&user.ID, &user.Username, &user.Admin, &user.CreatedAt)
	if err != nil {
		return jobs.User{}, err
	}
	user.CreatedAt = user.CreatedAt.UTC()
	return user, nil
}

func lockMetadata(ctx context.Context, tx *sql.Tx) error {
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM agenteval_metadata WHERE singleton=1 FOR UPDATE`).Scan(&owner); err != nil {
		return jobs.ErrUnavailable
	}
	return nil
}

func requireAdmin(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, actorID string) error {
	if !validID(actorID) {
		return jobs.ErrUnauthorized
	}
	var admin bool
	err := q.QueryRowContext(ctx, `SELECT is_admin FROM agenteval_users WHERE id=$1`, actorID).Scan(&admin)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.ErrUnauthorized
	}
	if err != nil {
		return jobs.ErrUnavailable
	}
	if !admin {
		return jobs.ErrForbidden
	}
	return nil
}

func actorExists(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, actorID string) (bool, error) {
	if !validID(actorID) {
		return false, jobs.ErrUnauthorized
	}
	var exists bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM agenteval_users WHERE id=$1)`, actorID).Scan(&exists)
	if err != nil {
		return false, jobs.ErrUnavailable
	}
	if !exists {
		return false, jobs.ErrUnauthorized
	}
	return true, nil
}

func (p *Postgres) ListUsers(ctx context.Context, actorID string) ([]jobs.User, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return nil, err
	}
	if err := requireAdmin(ctx, p.db, actorID); err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT id,username,is_admin,created_at FROM agenteval_users ORDER BY created_at,id`)
	if err != nil {
		return nil, jobs.ErrUnavailable
	}
	defer rows.Close()
	users := make([]jobs.User, 0)
	for rows.Next() {
		var user jobs.User
		if err := rows.Scan(&user.ID, &user.Username, &user.Admin, &user.CreatedAt); err != nil {
			return nil, jobs.ErrUnavailable
		}
		user.CreatedAt = user.CreatedAt.UTC()
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, jobs.ErrUnavailable
	}
	return users, nil
}

func (p *Postgres) Credentials(ctx context.Context, username string) (jobs.Credentials, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return jobs.Credentials{}, err
	}
	var credentials jobs.Credentials
	err := p.db.QueryRowContext(ctx, `SELECT id,username,is_admin,created_at,password_hash FROM agenteval_users WHERE username=$1`, strings.ToLower(strings.TrimSpace(username))).
		Scan(&credentials.User.ID, &credentials.User.Username, &credentials.User.Admin, &credentials.User.CreatedAt, &credentials.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Credentials{}, jobs.ErrUnauthorized
	}
	if err != nil {
		return jobs.Credentials{}, jobs.ErrUnavailable
	}
	credentials.User.CreatedAt = credentials.User.CreatedAt.UTC()
	return credentials, nil
}

func (p *Postgres) CreateSession(ctx context.Context, userID, tokenHash string) error {
	if !validID(userID) || !isHex(tokenHash, 32) {
		return jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO agenteval_sessions (token_hash,user_id,expires_at)
		VALUES ($1,$2,clock_timestamp()+interval '12 hours')`, tokenHash, userID)
	if err != nil {
		if uniqueViolation(err) {
			return jobs.ErrConflict
		}
		return jobs.ErrUnauthorized
	}
	return p.commitFencedTx(ctx, tx)
}

func (p *Postgres) Authenticate(ctx context.Context, tokenHash string) (jobs.User, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return jobs.User{}, err
	}
	if !isHex(tokenHash, 32) {
		return jobs.User{}, jobs.ErrUnauthorized
	}
	var user jobs.User
	err := p.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.is_admin,u.created_at
		FROM agenteval_sessions s JOIN agenteval_users u ON u.id=s.user_id
		WHERE s.token_hash=$1 AND s.expires_at>clock_timestamp()`, tokenHash).
		Scan(&user.ID, &user.Username, &user.Admin, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.User{}, jobs.ErrUnauthorized
	}
	if err != nil {
		return jobs.User{}, jobs.ErrUnavailable
	}
	user.CreatedAt = user.CreatedAt.UTC()
	return user, nil
}

func (p *Postgres) DeleteSession(ctx context.Context, tokenHash string) error {
	if !isHex(tokenHash, 32) {
		return jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM agenteval_sessions WHERE token_hash=$1`, tokenHash); err != nil {
		return jobs.ErrUnavailable
	}
	return p.commitFencedTx(ctx, tx)
}

type projectQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func projectForActor(ctx context.Context, q projectQueryer, actorID, projectID string) (jobs.Project, error) {
	if _, err := actorExists(ctx, q, actorID); err != nil {
		return jobs.Project{}, err
	}
	var project jobs.Project
	err := q.QueryRowContext(ctx, `SELECT p.id,p.name,p.daily_limit,p.concurrent_limit,p.created_at,
		COALESCE(m.role,'')
		FROM agenteval_projects p CROSS JOIN agenteval_users u
		LEFT JOIN agenteval_project_members m ON m.project_id=p.id AND m.user_id=u.id
		WHERE p.id=$1 AND u.id=$2`, projectID, actorID).
		Scan(&project.ID, &project.Name, &project.DailyLimit, &project.ConcurrentLimit, &project.CreatedAt, &project.Role)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && project.Role == "") {
		return jobs.Project{}, jobs.ErrForbidden
	}
	if err != nil {
		return jobs.Project{}, jobs.ErrUnavailable
	}
	project.CreatedAt = project.CreatedAt.UTC()
	return project, nil
}

func requireProjectRole(ctx context.Context, q projectQueryer, actorID, projectID string, roles ...string) (jobs.Project, error) {
	project, err := projectForActor(ctx, q, actorID, projectID)
	if err != nil {
		return jobs.Project{}, err
	}
	for _, role := range roles {
		if project.Role == role {
			return project, nil
		}
	}
	return jobs.Project{}, jobs.ErrForbidden
}

func (p *Postgres) CreateProject(ctx context.Context, actorID, name string, dailyLimit, concurrentLimit int) (jobs.Project, error) {
	if !validName(name, 120) || dailyLimit < 1 || dailyLimit > maxProjectDailyLimit || concurrentLimit < 1 || concurrentLimit > maxProjectConcurrentLimit {
		return jobs.Project{}, jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return jobs.Project{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := actorExists(ctx, tx, actorID); err != nil {
		return jobs.Project{}, err
	}
	id, err := randomID()
	if err != nil {
		return jobs.Project{}, jobs.ErrUnavailable
	}
	var project jobs.Project
	err = tx.QueryRowContext(ctx, `INSERT INTO agenteval_projects(id,name,created_by,daily_limit,concurrent_limit)
		VALUES($1,$2,$3,$4,$5) RETURNING id,name,daily_limit,concurrent_limit,created_at`, id, name, actorID, dailyLimit, concurrentLimit).
		Scan(&project.ID, &project.Name, &project.DailyLimit, &project.ConcurrentLimit, &project.CreatedAt)
	if err != nil {
		return jobs.Project{}, jobs.ErrUnavailable
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agenteval_project_members(project_id,user_id,role) VALUES($1,$2,'owner')`, id, actorID); err != nil {
		return jobs.Project{}, jobs.ErrUnavailable
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return jobs.Project{}, err
	}
	project.Role = "owner"
	project.CreatedAt = project.CreatedAt.UTC()
	return project, nil
}

func (p *Postgres) ListProjects(ctx context.Context, actorID string) ([]jobs.Project, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return nil, err
	}
	if _, err := actorExists(ctx, p.db, actorID); err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT p.id,p.name,p.daily_limit,p.concurrent_limit,p.created_at,m.role
		FROM agenteval_projects p JOIN agenteval_users u ON u.id=$1
		LEFT JOIN agenteval_project_members m ON m.project_id=p.id AND m.user_id=u.id
		WHERE m.user_id IS NOT NULL ORDER BY p.created_at DESC,p.id DESC`, actorID)
	if err != nil {
		return nil, jobs.ErrUnavailable
	}
	defer rows.Close()
	projects := make([]jobs.Project, 0)
	for rows.Next() {
		var project jobs.Project
		if err := rows.Scan(&project.ID, &project.Name, &project.DailyLimit, &project.ConcurrentLimit, &project.CreatedAt, &project.Role); err != nil {
			return nil, jobs.ErrUnavailable
		}
		project.CreatedAt = project.CreatedAt.UTC()
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, jobs.ErrUnavailable
	}
	return projects, nil
}

func (p *Postgres) GetProject(ctx context.Context, actorID, projectID string) (jobs.Project, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return jobs.Project{}, err
	}
	return projectForActor(ctx, p.db, actorID, projectID)
}

func (p *Postgres) UpdateProjectLimits(ctx context.Context, actorID, projectID string, daily, concurrent int) (jobs.Project, error) {
	if daily < 1 || daily > maxProjectDailyLimit || concurrent < 1 || concurrent > maxProjectConcurrentLimit {
		return jobs.Project{}, jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return jobs.Project{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockProject(ctx, tx, projectID); err != nil {
		return jobs.Project{}, err
	}
	project, err := requireProjectRole(ctx, tx, actorID, projectID, "owner")
	if err != nil {
		return jobs.Project{}, err
	}
	day, err := utcDay(ctx, tx)
	if err != nil {
		return jobs.Project{}, jobs.ErrUnavailable
	}
	var committed, running int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT reserved+used FROM agenteval_project_daily_usage WHERE project_id=$1 AND usage_day=$2::date),0),
		(SELECT count(*) FROM agenteval_jobs WHERE project_id=$1 AND status=$3)`, projectID, day, jobs.StatusRunning).Scan(&committed, &running); err != nil {
		return jobs.Project{}, jobs.ErrUnavailable
	}
	if committed > daily || running > concurrent {
		return jobs.Project{}, jobs.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agenteval_projects SET daily_limit=$2,concurrent_limit=$3 WHERE id=$1`, projectID, daily, concurrent); err != nil {
		return jobs.Project{}, jobs.ErrUnavailable
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return jobs.Project{}, err
	}
	project.DailyLimit, project.ConcurrentLimit = daily, concurrent
	return project, nil
}

func lockProject(ctx context.Context, tx *sql.Tx, projectID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM agenteval_projects WHERE id=$1 FOR UPDATE`, projectID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.ErrForbidden
	}
	if err != nil {
		return jobs.ErrUnavailable
	}
	return nil
}

func (p *Postgres) ListMembers(ctx context.Context, actorID, projectID string) ([]jobs.Member, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return nil, err
	}
	if _, err := requireProjectRole(ctx, p.db, actorID, projectID, "owner", "executor", "reader"); err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT u.id,u.username,m.role FROM agenteval_project_members m
		JOIN agenteval_users u ON u.id=m.user_id WHERE m.project_id=$1 ORDER BY m.role,u.username`, projectID)
	if err != nil {
		return nil, jobs.ErrUnavailable
	}
	defer rows.Close()
	members := make([]jobs.Member, 0)
	for rows.Next() {
		var member jobs.Member
		if err := rows.Scan(&member.UserID, &member.Username, &member.Role); err != nil {
			return nil, jobs.ErrUnavailable
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, jobs.ErrUnavailable
	}
	return members, nil
}

func (p *Postgres) SetMember(ctx context.Context, actorID, projectID, username, role string) error {
	if !validUsername(strings.ToLower(strings.TrimSpace(username))) || !validRole(role) {
		return jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockProject(ctx, tx, projectID); err != nil {
		return err
	}
	if _, err := requireProjectRole(ctx, tx, actorID, projectID, "owner"); err != nil {
		return err
	}
	var userID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM agenteval_users WHERE username=$1`, strings.ToLower(strings.TrimSpace(username))).Scan(&userID); errors.Is(err, sql.ErrNoRows) {
		return jobs.ErrNotFound
	} else if err != nil {
		return jobs.ErrUnavailable
	}
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT role FROM agenteval_project_members WHERE project_id=$1 AND user_id=$2 FOR UPDATE`, projectID, userID).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return jobs.ErrUnavailable
	}
	if previous == "owner" && role != "owner" {
		var owners int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agenteval_project_members WHERE project_id=$1 AND role='owner'`, projectID).Scan(&owners); err != nil {
			return jobs.ErrUnavailable
		}
		if owners <= 1 {
			return jobs.ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agenteval_project_members(project_id,user_id,role) VALUES($1,$2,$3)
		ON CONFLICT(project_id,user_id) DO UPDATE SET role=EXCLUDED.role`, projectID, userID, role); err != nil {
		return jobs.ErrUnavailable
	}
	return p.commitFencedTx(ctx, tx)
}

func (p *Postgres) RemoveMember(ctx context.Context, actorID, projectID, userID string) error {
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockProject(ctx, tx, projectID); err != nil {
		return err
	}
	if _, err := requireProjectRole(ctx, tx, actorID, projectID, "owner"); err != nil {
		return err
	}
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM agenteval_project_members WHERE project_id=$1 AND user_id=$2 FOR UPDATE`, projectID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.ErrNotFound
	}
	if err != nil {
		return jobs.ErrUnavailable
	}
	if role == "owner" {
		var owners int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agenteval_project_members WHERE project_id=$1 AND role='owner'`, projectID).Scan(&owners); err != nil {
			return jobs.ErrUnavailable
		}
		if owners <= 1 {
			return jobs.ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agenteval_project_members WHERE project_id=$1 AND user_id=$2`, projectID, userID); err != nil {
		return jobs.ErrUnavailable
	}
	return p.commitFencedTx(ctx, tx)
}

func validDatasetTasks(tasks []jobs.DatasetTask) error {
	if len(tasks) == 0 || len(tasks) > maxDatasetTasks {
		return jobs.ErrInvalidRequest
	}
	seen := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		if task.TaskID == "" || task.Snapshot.TaskRevision < 1 || task.Snapshot.ImageID == "" ||
			!isHex(task.Snapshot.ManifestSHA256, 32) || !isHex(task.Snapshot.SourceSHA256, 32) ||
			!isHex(task.Snapshot.EvaluatorSHA256, 32) ||
			(task.Snapshot.AgentConfigSHA256 != "" && !isHex(task.Snapshot.AgentConfigSHA256, 32)) {
			return jobs.ErrInvalidRequest
		}
		if task.Snapshot.Backend == "harbor" && (!isHex(task.Snapshot.PackageSHA256, 32) || !strings.HasPrefix(task.Snapshot.VerifierImageID, "sha256:") || !isHex(strings.TrimPrefix(task.Snapshot.VerifierImageID, "sha256:"), 32)) {
			return jobs.ErrInvalidRequest
		}
		if _, ok := seen[task.TaskID]; ok {
			return jobs.ErrInvalidRequest
		}
		seen[task.TaskID] = struct{}{}
	}
	return nil
}

func (p *Postgres) CreateDatasetVersion(ctx context.Context, actorID, projectID, name, version string, tasks []jobs.DatasetTask) (jobs.DatasetVersion, error) {
	if !validName(name, 120) || !validName(version, 80) || validDatasetTasks(tasks) != nil {
		return jobs.DatasetVersion{}, jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return jobs.DatasetVersion{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockProject(ctx, tx, projectID); err != nil {
		return jobs.DatasetVersion{}, err
	}
	if _, err := requireProjectRole(ctx, tx, actorID, projectID, "owner"); err != nil {
		return jobs.DatasetVersion{}, err
	}
	var isAdmin bool
	if err := tx.QueryRowContext(ctx, `SELECT is_admin FROM agenteval_users WHERE id=$1`, actorID).Scan(&isAdmin); err != nil {
		return jobs.DatasetVersion{}, jobs.ErrUnauthorized
	}
	if !isAdmin {
		for _, task := range tasks {
			var granted bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
				SELECT 1 FROM agenteval_dataset_versions v
				CROSS JOIN LATERAL jsonb_array_elements(v.tasks) t
				WHERE v.project_id=$1 AND t->>'task_id'=$2)`, projectID, task.TaskID).Scan(&granted); err != nil {
				return jobs.DatasetVersion{}, jobs.ErrUnavailable
			}
			if !granted {
				return jobs.DatasetVersion{}, jobs.ErrForbidden
			}
		}
	}
	id, err := randomID()
	if err != nil {
		return jobs.DatasetVersion{}, jobs.ErrUnavailable
	}
	tasksJSON, err := json.Marshal(tasks)
	if err != nil {
		return jobs.DatasetVersion{}, jobs.ErrInvalidRequest
	}
	var dataset jobs.DatasetVersion
	err = tx.QueryRowContext(ctx, `INSERT INTO agenteval_dataset_versions(id,project_id,name,version,created_by,tasks)
		VALUES($1,$2,$3,$4,$5,$6::jsonb) RETURNING id,project_id,name,version,created_by,tasks,created_at`, id, projectID, name, version, actorID, tasksJSON).
		Scan(&dataset.ID, &dataset.ProjectID, &dataset.Name, &dataset.Version, &dataset.CreatedBy, &tasksJSON, &dataset.CreatedAt)
	if err != nil {
		if uniqueViolation(err) {
			return jobs.DatasetVersion{}, jobs.ErrConflict
		}
		return jobs.DatasetVersion{}, jobs.ErrUnavailable
	}
	if err := json.Unmarshal(tasksJSON, &dataset.Tasks); err != nil {
		return jobs.DatasetVersion{}, jobs.ErrUnavailable
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return jobs.DatasetVersion{}, err
	}
	dataset.CreatedAt = dataset.CreatedAt.UTC()
	return dataset, nil
}

func (p *Postgres) ListDatasetVersions(ctx context.Context, actorID, projectID string) ([]jobs.DatasetVersion, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return nil, err
	}
	if _, err := requireProjectRole(ctx, p.db, actorID, projectID, "owner", "executor", "reader"); err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT id,project_id,name,version,created_by,tasks,created_at FROM agenteval_dataset_versions
		WHERE project_id=$1 ORDER BY created_at DESC,id DESC`, projectID)
	if err != nil {
		return nil, jobs.ErrUnavailable
	}
	defer rows.Close()
	versions := make([]jobs.DatasetVersion, 0)
	for rows.Next() {
		var item jobs.DatasetVersion
		var data []byte
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.Name, &item.Version, &item.CreatedBy, &data, &item.CreatedAt); err != nil || json.Unmarshal(data, &item.Tasks) != nil {
			return nil, jobs.ErrUnavailable
		}
		item.CreatedAt = item.CreatedAt.UTC()
		versions = append(versions, item)
	}
	if err := rows.Err(); err != nil {
		return nil, jobs.ErrUnavailable
	}
	return versions, nil
}

func (p *Postgres) GetDatasetVersion(ctx context.Context, actorID, projectID, id string) (jobs.DatasetVersion, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return jobs.DatasetVersion{}, err
	}
	if _, err := requireProjectRole(ctx, p.db, actorID, projectID, "owner", "executor", "reader"); err != nil {
		return jobs.DatasetVersion{}, err
	}
	var item jobs.DatasetVersion
	var data []byte
	err := p.db.QueryRowContext(ctx, `SELECT id,project_id,name,version,created_by,tasks,created_at FROM agenteval_dataset_versions WHERE project_id=$1 AND id=$2`, projectID, id).
		Scan(&item.ID, &item.ProjectID, &item.Name, &item.Version, &item.CreatedBy, &data, &item.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.DatasetVersion{}, jobs.ErrNotFound
	}
	if err != nil || json.Unmarshal(data, &item.Tasks) != nil {
		return jobs.DatasetVersion{}, jobs.ErrUnavailable
	}
	item.CreatedAt = item.CreatedAt.UTC()
	return item, nil
}

var _ jobs.ManagementStore = (*Postgres)(nil)

func hashJSON(value any) (string, []byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), data, nil
}

func utcDay(ctx context.Context, tx *sql.Tx) (string, error) {
	var day string
	err := tx.QueryRowContext(ctx, `SELECT to_char((clock_timestamp() AT TIME ZONE 'UTC')::date,'YYYY-MM-DD')`).Scan(&day)
	return day, err
}

func updateReservation(ctx context.Context, tx *sql.Tx, jobID, expected, next string) error {
	var projectID, day string
	err := tx.QueryRowContext(ctx, `UPDATE agenteval_job_reservations SET state=$3,updated_at=clock_timestamp()
		WHERE job_id=$1 AND state=$2 RETURNING project_id,to_char(usage_day,'YYYY-MM-DD')`, jobID, expected, next).Scan(&projectID, &day)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return jobs.ErrUnavailable
	}
	var usageErr error
	switch {
	case expected == "reserved" && next == "used":
		_, usageErr = tx.ExecContext(ctx, `UPDATE agenteval_project_daily_usage SET reserved=reserved-1,used=used+1 WHERE project_id=$1 AND usage_day=$2::date`, projectID, day)
	case expected == "reserved" && next == "released":
		_, usageErr = tx.ExecContext(ctx, `UPDATE agenteval_project_daily_usage SET reserved=reserved-1,released=released+1 WHERE project_id=$1 AND usage_day=$2::date`, projectID, day)
	default:
		return jobs.ErrInvalidRequest
	}
	if usageErr != nil {
		return jobs.ErrUnavailable
	}
	return nil
}

func scanExperiment(row interface{ Scan(...any) error }) (jobs.Experiment, error) {
	var item jobs.Experiment
	err := row.Scan(&item.ID, &item.ProjectID, &item.DatasetVersionID, &item.Name, &item.CreatedBy, &item.IdempotencyKey, &item.CreatedAt)
	item.CreatedAt = item.CreatedAt.UTC()
	return item, err
}

const experimentColumns = `id,project_id,dataset_version_id,name,created_by,idempotency_key,created_at`

func validateExperimentItems(items []jobs.PreparedJob) ([]jobs.PreparedJob, error) {
	if len(items) == 0 || len(items) > maxExperimentJobs {
		return nil, jobs.ErrInvalidRequest
	}
	prepared := make([]jobs.PreparedJob, len(items))
	for i, item := range items {
		if item.Request.ExpectedSnapshot != nil && *item.Request.ExpectedSnapshot != item.Snapshot {
			return nil, jobs.ErrConflict
		}
		item.Request.ExpectedSnapshot = nil
		if item.Request.SchemaVersion != 1 || strings.TrimSpace(item.Request.TaskID) == "" ||
			(item.Request.Agent != "scripted" && item.Request.Agent != "llm") ||
			(item.Request.Agent == "scripted" && item.Request.Script == "") ||
			(item.Request.Agent == "llm" && item.Request.Script != "") ||
			item.Snapshot.TaskRevision < 1 || item.Snapshot.ImageID == "" ||
			!isHex(item.Snapshot.ManifestSHA256, 32) || !isHex(item.Snapshot.SourceSHA256, 32) ||
			!isHex(item.Snapshot.EvaluatorSHA256, 32) || !isHex(item.Snapshot.AgentConfigSHA256, 32) {
			return nil, jobs.ErrInvalidRequest
		}
		prepared[i] = item
	}
	return prepared, nil
}

func snapshotsMatchDataset(actual, registered jobs.TaskSnapshot) bool {
	actual.AgentConfigSHA256 = registered.AgentConfigSHA256
	return actual == registered
}

func (p *Postgres) SubmitExperiment(ctx context.Context, actorID, projectID, datasetVersionID, name, key string, items []jobs.PreparedJob, capacity int) (jobs.Experiment, bool, error) {
	if !validName(name, 120) || !jobs.ValidIdempotencyKey(key) || key == "" || capacity < 0 {
		return jobs.Experiment{}, false, jobs.ErrInvalidRequest
	}
	prepared, err := validateExperimentItems(items)
	if err != nil {
		return jobs.Experiment{}, false, err
	}
	hashPayload := struct {
		ProjectID string             `json:"project_id"`
		DatasetID string             `json:"dataset_version_id"`
		Name      string             `json:"name"`
		Items     []jobs.PreparedJob `json:"items"`
	}{projectID, datasetVersionID, name, prepared}
	digest, _, err := hashJSON(hashPayload)
	if err != nil {
		return jobs.Experiment{}, false, jobs.ErrInvalidRequest
	}
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return jobs.Experiment{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	project, err := lockProjectAndAuthorize(ctx, tx, actorID, projectID, "owner", "executor")
	if err != nil {
		return jobs.Experiment{}, false, err
	}
	var datasetData []byte
	err = tx.QueryRowContext(ctx, `SELECT tasks FROM agenteval_dataset_versions WHERE id=$1 AND project_id=$2`, datasetVersionID, projectID).Scan(&datasetData)
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Experiment{}, false, jobs.ErrForbidden
	}
	if err != nil {
		return jobs.Experiment{}, false, jobs.ErrUnavailable
	}
	var registered []jobs.DatasetTask
	if json.Unmarshal(datasetData, &registered) != nil {
		return jobs.Experiment{}, false, jobs.ErrUnavailable
	}
	byTask := make(map[string]jobs.TaskSnapshot, len(registered))
	for _, task := range registered {
		byTask[task.TaskID] = task.Snapshot
	}
	for _, item := range prepared {
		baseline, ok := byTask[item.Request.TaskID]
		if !ok || !snapshotsMatchDataset(item.Snapshot, baseline) {
			return jobs.Experiment{}, false, jobs.ErrForbidden
		}
	}
	existing, findErr := scanExperiment(tx.QueryRowContext(ctx, `SELECT `+experimentColumns+`
		FROM agenteval_experiments WHERE project_id=$1 AND idempotency_key=$2`, projectID, key))
	if findErr == nil {
		var existingDigest string
		if err := tx.QueryRowContext(ctx, `SELECT request_sha256 FROM agenteval_experiments WHERE id=$1`, existing.ID).Scan(&existingDigest); err != nil {
			return jobs.Experiment{}, false, jobs.ErrUnavailable
		}
		if existingDigest != digest {
			return jobs.Experiment{}, false, jobs.ErrConflict
		}
		if err := loadExperimentJobs(ctx, tx, &existing); err != nil {
			return jobs.Experiment{}, false, err
		}
		if err := p.commitFencedTx(ctx, tx); err != nil {
			return jobs.Experiment{}, false, err
		}
		return existing, true, nil
	}
	if !errors.Is(findErr, sql.ErrNoRows) {
		return jobs.Experiment{}, false, jobs.ErrUnavailable
	}
	if err := lockMetadata(ctx, tx); err != nil {
		return jobs.Experiment{}, false, err
	}
	if capacity > 0 {
		var queued int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agenteval_jobs WHERE status=$1`, jobs.StatusQueued).Scan(&queued); err != nil {
			return jobs.Experiment{}, false, jobs.ErrUnavailable
		}
		if queued+len(prepared) > capacity {
			return jobs.Experiment{}, false, jobs.ErrQueueFull
		}
	}
	day, err := utcDay(ctx, tx)
	if err != nil {
		return jobs.Experiment{}, false, jobs.ErrUnavailable
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agenteval_project_daily_usage(project_id,usage_day) VALUES($1,$2::date)
		ON CONFLICT(project_id,usage_day) DO NOTHING`, projectID, day); err != nil {
		return jobs.Experiment{}, false, jobs.ErrUnavailable
	}
	var reserved, used int
	if err := tx.QueryRowContext(ctx, `SELECT reserved,used FROM agenteval_project_daily_usage
		WHERE project_id=$1 AND usage_day=$2::date FOR UPDATE`, projectID, day).Scan(&reserved, &used); err != nil {
		return jobs.Experiment{}, false, jobs.ErrUnavailable
	}
	if reserved+used+len(prepared) > project.DailyLimit {
		return jobs.Experiment{}, false, jobs.ErrQuotaExceeded
	}
	experimentID, err := randomID()
	if err != nil {
		return jobs.Experiment{}, false, jobs.ErrUnavailable
	}
	var created jobs.Experiment
	err = tx.QueryRowContext(ctx, `INSERT INTO agenteval_experiments
		(id,project_id,dataset_version_id,name,created_by,idempotency_key,request_sha256)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+experimentColumns,
		experimentID, projectID, datasetVersionID, name, actorID, key, digest).
		Scan(&created.ID, &created.ProjectID, &created.DatasetVersionID, &created.Name, &created.CreatedBy, &created.IdempotencyKey, &created.CreatedAt)
	if err != nil {
		if uniqueViolation(err) {
			return jobs.Experiment{}, false, jobs.ErrConflict
		}
		return jobs.Experiment{}, false, jobs.ErrUnavailable
	}
	for index, item := range prepared {
		jobID, err := randomID()
		if err != nil {
			return jobs.Experiment{}, false, jobs.ErrUnavailable
		}
		jobDigest, _, err := hashJSON(struct {
			Request  jobs.Request      `json:"request"`
			Snapshot jobs.TaskSnapshot `json:"task_snapshot"`
		}{item.Request, item.Snapshot})
		if err != nil {
			return jobs.Experiment{}, false, jobs.ErrInvalidRequest
		}
		requestJSON, _ := json.Marshal(item.Request)
		snapshotJSON, _ := json.Marshal(item.Snapshot)
		_, err = tx.ExecContext(ctx, `INSERT INTO agenteval_jobs
			(id,status,request,task_snapshot,request_sha256,project_id,experiment_id,dataset_version_id,created_by,experiment_index)
			VALUES($1,$2,$3::jsonb,$4::jsonb,$5,$6,$7,$8,$9,$10)`, jobID, jobs.StatusQueued,
			requestJSON, snapshotJSON, jobDigest, projectID, experimentID, datasetVersionID, actorID, index)
		if err != nil {
			return jobs.Experiment{}, false, jobs.ErrUnavailable
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agenteval_job_reservations(job_id,project_id,usage_day,state) VALUES($1,$2,$3::date,'reserved')`, jobID, projectID, day); err != nil {
			return jobs.Experiment{}, false, jobs.ErrUnavailable
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agenteval_project_daily_usage SET reserved=reserved+$3 WHERE project_id=$1 AND usage_day=$2::date`, projectID, day, len(prepared)); err != nil {
		return jobs.Experiment{}, false, jobs.ErrUnavailable
	}
	if err := loadExperimentJobs(ctx, tx, &created); err != nil {
		return jobs.Experiment{}, false, err
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return jobs.Experiment{}, false, err
	}
	created.CreatedAt = created.CreatedAt.UTC()
	return created, false, nil
}

func lockProjectAndAuthorize(ctx context.Context, tx *sql.Tx, actorID, projectID string, roles ...string) (jobs.Project, error) {
	if err := lockProject(ctx, tx, projectID); err != nil {
		return jobs.Project{}, err
	}
	return requireProjectRole(ctx, tx, actorID, projectID, roles...)
}

func loadExperimentJobs(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, experiment *jobs.Experiment) error {
	rows, err := q.QueryContext(ctx, `SELECT `+jobColumns+` FROM agenteval_jobs WHERE experiment_id=$1 ORDER BY experiment_index`, experiment.ID)
	if err != nil {
		return jobs.ErrUnavailable
	}
	defer rows.Close()
	experiment.Jobs = make([]jobs.Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return jobs.ErrUnavailable
		}
		experiment.Jobs = append(experiment.Jobs, job)
	}
	if err := rows.Err(); err != nil {
		return jobs.ErrUnavailable
	}
	setExperimentSummary(experiment)
	return nil
}

func setExperimentSummary(experiment *jobs.Experiment) {
	experiment.JobCount = len(experiment.Jobs)
	switch {
	case anyJobStatus(experiment.Jobs, jobs.StatusRunning):
		experiment.State = jobs.StatusRunning
	case anyJobStatus(experiment.Jobs, jobs.StatusQueued):
		experiment.State = jobs.StatusQueued
	case anyJobStatus(experiment.Jobs, jobs.StatusInfraError):
		experiment.State = jobs.StatusInfraError
	case anyJobStatus(experiment.Jobs, jobs.StatusFailed) || anyJobStatus(experiment.Jobs, jobs.StatusTimedOut):
		experiment.State = jobs.StatusFailed
	case anyJobStatus(experiment.Jobs, jobs.StatusCanceled):
		experiment.State = jobs.StatusCanceled
	default:
		experiment.State = jobs.StatusSucceeded
	}
}

func anyJobStatus(items []jobs.Job, status string) bool {
	for _, item := range items {
		if item.Status == status {
			return true
		}
	}
	return false
}

func (p *Postgres) GetExperiment(ctx context.Context, actorID, projectID, id string) (jobs.Experiment, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return jobs.Experiment{}, err
	}
	if _, err := requireProjectRole(ctx, p.db, actorID, projectID, "owner", "executor", "reader"); err != nil {
		return jobs.Experiment{}, err
	}
	experiment, err := scanExperiment(p.db.QueryRowContext(ctx, `SELECT `+experimentColumns+` FROM agenteval_experiments WHERE project_id=$1 AND id=$2`, projectID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Experiment{}, jobs.ErrNotFound
	}
	if err != nil {
		return jobs.Experiment{}, jobs.ErrUnavailable
	}
	if err := loadExperimentJobs(ctx, p.db, &experiment); err != nil {
		return jobs.Experiment{}, err
	}
	return experiment, nil
}

func (p *Postgres) ListExperiments(ctx context.Context, actorID, projectID string) ([]jobs.Experiment, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return nil, err
	}
	if _, err := requireProjectRole(ctx, p.db, actorID, projectID, "owner", "executor", "reader"); err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT e.id,e.project_id,e.dataset_version_id,e.name,e.created_by,e.idempotency_key,e.created_at,
		count(j.id)::integer,
		CASE
			WHEN count(j.id) FILTER (WHERE j.status='RUNNING')>0 THEN 'RUNNING'
			WHEN count(j.id) FILTER (WHERE j.status='QUEUED')>0 THEN 'QUEUED'
			WHEN count(j.id) FILTER (WHERE j.status='INFRA_ERROR')>0 THEN 'INFRA_ERROR'
			WHEN count(j.id) FILTER (WHERE j.status IN ('FAILED','TIMED_OUT'))>0 THEN 'FAILED'
			WHEN count(j.id) FILTER (WHERE j.status='CANCELED')>0 THEN 'CANCELED'
			ELSE 'SUCCEEDED'
		END
		FROM agenteval_experiments e LEFT JOIN agenteval_jobs j ON j.experiment_id=e.id
		WHERE e.project_id=$1 GROUP BY e.id ORDER BY e.created_at DESC,e.id DESC LIMIT 100`, projectID)
	if err != nil {
		return nil, jobs.ErrUnavailable
	}
	defer rows.Close()
	items := make([]jobs.Experiment, 0)
	for rows.Next() {
		var item jobs.Experiment
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.DatasetVersionID, &item.Name, &item.CreatedBy, &item.IdempotencyKey, &item.CreatedAt, &item.JobCount, &item.State); err != nil {
			return nil, jobs.ErrUnavailable
		}
		item.CreatedAt = item.CreatedAt.UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, jobs.ErrUnavailable
	}
	return items, nil
}

func (p *Postgres) ProjectJobs(ctx context.Context, actorID, projectID string) ([]jobs.Job, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return nil, err
	}
	if _, err := requireProjectRole(ctx, p.db, actorID, projectID, "owner", "executor", "reader"); err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM agenteval_jobs WHERE project_id=$1 ORDER BY created_at DESC,id DESC LIMIT 100`, projectID)
	if err != nil {
		return nil, jobs.ErrUnavailable
	}
	defer rows.Close()
	items := make([]jobs.Job, 0)
	for rows.Next() {
		item, err := scanJob(rows)
		if err != nil {
			return nil, jobs.ErrUnavailable
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, jobs.ErrUnavailable
	}
	return items, nil
}

func (p *Postgres) GetProjectJob(ctx context.Context, actorID, projectID, id string) (jobs.Job, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return jobs.Job{}, err
	}
	if _, err := requireProjectRole(ctx, p.db, actorID, projectID, "owner", "executor", "reader"); err != nil {
		return jobs.Job{}, err
	}
	job, err := scanJob(p.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM agenteval_jobs WHERE project_id=$1 AND id=$2`, projectID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, jobs.ErrUnavailable
	}
	return job, nil
}

func (p *Postgres) CancelProjectJob(ctx context.Context, actorID, projectID, id string) (jobs.Job, error) {
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return jobs.Job{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := lockProjectAndAuthorize(ctx, tx, actorID, projectID, "owner", "executor"); err != nil {
		return jobs.Job{}, err
	}
	job, err := cancelProjectJobTx(ctx, tx, projectID, id)
	if err != nil {
		return jobs.Job{}, err
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return jobs.Job{}, err
	}
	return job, nil
}

func cancelProjectJobTx(ctx context.Context, tx *sql.Tx, projectID, id string) (jobs.Job, error) {
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM agenteval_jobs WHERE project_id=$1 AND id=$2 FOR UPDATE`, projectID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, jobs.ErrUnavailable
	}
	switch job.Status {
	case jobs.StatusQueued:
		if _, err := tx.ExecContext(ctx, `UPDATE agenteval_jobs SET status=$2,cancel_requested=true,reason='canceled',
			finished_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1 AND status=$3`, id, jobs.StatusCanceled, jobs.StatusQueued); err != nil {
			return jobs.Job{}, jobs.ErrUnavailable
		}
		if err := updateReservation(ctx, tx, id, "reserved", "released"); err != nil {
			return jobs.Job{}, err
		}
	case jobs.StatusRunning:
		if _, err := tx.ExecContext(ctx, `UPDATE agenteval_jobs SET cancel_requested=true,updated_at=clock_timestamp() WHERE id=$1 AND status=$2`, id, jobs.StatusRunning); err != nil {
			return jobs.Job{}, jobs.ErrUnavailable
		}
	}
	job, err = scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM agenteval_jobs WHERE id=$1`, id))
	if err != nil {
		return jobs.Job{}, jobs.ErrUnavailable
	}
	return job, nil
}

func (p *Postgres) CancelExperiment(ctx context.Context, actorID, projectID, id string) (jobs.Experiment, error) {
	tx, err := p.beginFencedTx(ctx)
	if err != nil {
		return jobs.Experiment{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := lockProjectAndAuthorize(ctx, tx, actorID, projectID, "owner", "executor"); err != nil {
		return jobs.Experiment{}, err
	}
	experiment, err := scanExperiment(tx.QueryRowContext(ctx, `SELECT `+experimentColumns+` FROM agenteval_experiments WHERE project_id=$1 AND id=$2`, projectID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return jobs.Experiment{}, jobs.ErrNotFound
	}
	if err != nil {
		return jobs.Experiment{}, jobs.ErrUnavailable
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM agenteval_jobs WHERE experiment_id=$1 AND status IN ($2,$3) ORDER BY experiment_index FOR UPDATE`, id, jobs.StatusQueued, jobs.StatusRunning)
	if err != nil {
		return jobs.Experiment{}, jobs.ErrUnavailable
	}
	ids := make([]string, 0)
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			_ = rows.Close()
			return jobs.Experiment{}, jobs.ErrUnavailable
		}
		ids = append(ids, jobID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return jobs.Experiment{}, jobs.ErrUnavailable
	}
	_ = rows.Close()
	for _, jobID := range ids {
		if _, err := cancelProjectJobTx(ctx, tx, projectID, jobID); err != nil {
			return jobs.Experiment{}, err
		}
	}
	if err := loadExperimentJobs(ctx, tx, &experiment); err != nil {
		return jobs.Experiment{}, err
	}
	if err := p.commitFencedTx(ctx, tx); err != nil {
		return jobs.Experiment{}, err
	}
	return experiment, nil
}

func (p *Postgres) ProjectUsage(ctx context.Context, actorID, projectID, day string) (jobs.Usage, error) {
	if err := p.ensureLeader(ctx); err != nil {
		return jobs.Usage{}, err
	}
	project, err := requireProjectRole(ctx, p.db, actorID, projectID, "owner", "executor", "reader")
	if err != nil {
		return jobs.Usage{}, err
	}
	if day == "" {
		if err := p.db.QueryRowContext(ctx, `SELECT to_char((clock_timestamp() AT TIME ZONE 'UTC')::date,'YYYY-MM-DD')`).Scan(&day); err != nil {
			return jobs.Usage{}, jobs.ErrUnavailable
		}
	}
	if len(day) != len("2006-01-02") {
		return jobs.Usage{}, jobs.ErrInvalidRequest
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return jobs.Usage{}, jobs.ErrInvalidRequest
	}
	usage := jobs.Usage{ProjectID: projectID, Day: day, DailyLimit: project.DailyLimit, ConcurrentLimit: project.ConcurrentLimit}
	err = p.db.QueryRowContext(ctx, `SELECT reserved,used,released FROM agenteval_project_daily_usage WHERE project_id=$1 AND usage_day=$2::date`, projectID, day).Scan(&usage.Reserved, &usage.Used, &usage.Released)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return jobs.Usage{}, jobs.ErrUnavailable
	}
	if err := p.db.QueryRowContext(ctx, `SELECT count(*) FROM agenteval_jobs WHERE project_id=$1 AND status=$2`, projectID, jobs.StatusRunning).Scan(&usage.Running); err != nil {
		return jobs.Usage{}, jobs.ErrUnavailable
	}
	return usage, nil
}
