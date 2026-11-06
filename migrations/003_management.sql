CREATE TABLE IF NOT EXISTS agenteval_users (
    id TEXT PRIMARY KEY CHECK (id ~ '^[a-f0-9]{24}$'),
    username TEXT NOT NULL UNIQUE CHECK (length(username) BETWEEN 3 AND 64 AND username = lower(username)),
    password_hash TEXT NOT NULL CHECK (length(password_hash) BETWEEN 1 AND 512),
    is_admin BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS agenteval_sessions (
    token_hash TEXT PRIMARY KEY CHECK (token_hash ~ '^[a-f0-9]{64}$'),
    user_id TEXT NOT NULL REFERENCES agenteval_users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS agenteval_sessions_expiry ON agenteval_sessions (expires_at);

CREATE TABLE IF NOT EXISTS agenteval_projects (
    id TEXT PRIMARY KEY CHECK (id ~ '^[a-f0-9]{24}$'),
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    created_by TEXT NOT NULL REFERENCES agenteval_users(id),
    daily_limit INTEGER NOT NULL CHECK (daily_limit BETWEEN 1 AND 1000000),
    concurrent_limit INTEGER NOT NULL CHECK (concurrent_limit BETWEEN 1 AND 1000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (id, created_by)
);

CREATE TABLE IF NOT EXISTS agenteval_project_members (
    project_id TEXT NOT NULL REFERENCES agenteval_projects(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES agenteval_users(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('owner','executor','reader')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX IF NOT EXISTS agenteval_project_members_user ON agenteval_project_members (user_id, project_id);

CREATE TABLE IF NOT EXISTS agenteval_dataset_versions (
    id TEXT PRIMARY KEY CHECK (id ~ '^[a-f0-9]{24}$'),
    project_id TEXT NOT NULL REFERENCES agenteval_projects(id),
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    version TEXT NOT NULL CHECK (length(version) BETWEEN 1 AND 80),
    created_by TEXT NOT NULL REFERENCES agenteval_users(id),
    tasks JSONB NOT NULL CHECK (jsonb_typeof(tasks) = 'array'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (project_id, name, version),
    UNIQUE (id, project_id)
);
CREATE INDEX IF NOT EXISTS agenteval_dataset_versions_project ON agenteval_dataset_versions (project_id, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS agenteval_experiments (
    id TEXT PRIMARY KEY CHECK (id ~ '^[a-f0-9]{24}$'),
    project_id TEXT NOT NULL REFERENCES agenteval_projects(id),
    dataset_version_id TEXT NOT NULL,
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    created_by TEXT NOT NULL REFERENCES agenteval_users(id),
    idempotency_key TEXT NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[a-f0-9]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (project_id, idempotency_key),
    UNIQUE (id, project_id),
    FOREIGN KEY (dataset_version_id, project_id) REFERENCES agenteval_dataset_versions (id, project_id)
);
CREATE INDEX IF NOT EXISTS agenteval_experiments_project ON agenteval_experiments (project_id, created_at DESC, id DESC);

ALTER TABLE agenteval_jobs ADD COLUMN IF NOT EXISTS project_id TEXT REFERENCES agenteval_projects(id);
ALTER TABLE agenteval_jobs ADD COLUMN IF NOT EXISTS experiment_id TEXT REFERENCES agenteval_experiments(id);
ALTER TABLE agenteval_jobs ADD COLUMN IF NOT EXISTS dataset_version_id TEXT REFERENCES agenteval_dataset_versions(id);
ALTER TABLE agenteval_jobs ADD COLUMN IF NOT EXISTS created_by TEXT REFERENCES agenteval_users(id);
ALTER TABLE agenteval_jobs ADD COLUMN IF NOT EXISTS experiment_index INTEGER;
CREATE INDEX IF NOT EXISTS agenteval_jobs_project_created ON agenteval_jobs (project_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS agenteval_jobs_project_running ON agenteval_jobs (project_id) WHERE status = 'RUNNING';
CREATE INDEX IF NOT EXISTS agenteval_jobs_experiment_order ON agenteval_jobs (experiment_id, experiment_index);

CREATE TABLE IF NOT EXISTS agenteval_project_daily_usage (
    project_id TEXT NOT NULL REFERENCES agenteval_projects(id) ON DELETE CASCADE,
    usage_day DATE NOT NULL,
    reserved INTEGER NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    used INTEGER NOT NULL DEFAULT 0 CHECK (used >= 0),
    released INTEGER NOT NULL DEFAULT 0 CHECK (released >= 0),
    PRIMARY KEY (project_id, usage_day)
);

CREATE TABLE IF NOT EXISTS agenteval_job_reservations (
    job_id TEXT PRIMARY KEY REFERENCES agenteval_jobs(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL,
    usage_day DATE NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('reserved','used','released')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (project_id, usage_day) REFERENCES agenteval_project_daily_usage (project_id, usage_day)
);
CREATE INDEX IF NOT EXISTS agenteval_job_reservations_project_day ON agenteval_job_reservations (project_id, usage_day, state);
