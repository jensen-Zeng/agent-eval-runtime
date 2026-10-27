CREATE TABLE IF NOT EXISTS agenteval_metadata (
    singleton SMALLINT PRIMARY KEY CHECK (singleton = 1),
    owner_id TEXT NOT NULL CHECK (owner_id ~ '^[a-f0-9]{24}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS agenteval_jobs (
    id TEXT PRIMARY KEY CHECK (id ~ '^[a-f0-9]{24}$'),
    status TEXT NOT NULL CHECK (status IN ('QUEUED','RUNNING','SUCCEEDED','FAILED','TIMED_OUT','CANCELED','INFRA_ERROR')),
    request JSONB NOT NULL CHECK (jsonb_typeof(request) = 'object'),
    task_snapshot JSONB NOT NULL CHECK (jsonb_typeof(task_snapshot) = 'object'),
    idempotency_key TEXT NULL CHECK (idempotency_key IS NULL OR (length(idempotency_key) BETWEEN 1 AND 255)),
    request_sha256 TEXT NOT NULL CHECK (request_sha256 ~ '^[a-f0-9]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ NULL,
    finished_at TIMESTAMPTZ NULL,
    cancel_requested BOOLEAN NOT NULL DEFAULT false,
    owner_token TEXT NULL,
    run_id TEXT NULL CHECK (run_id IS NULL OR run_id ~ '^[a-f0-9]{24}$'),
    attempt_id TEXT NULL,
    result JSONB NULL CHECK (result IS NULL OR jsonb_typeof(result) = 'object'),
    reason TEXT NULL,
    CONSTRAINT agenteval_jobs_idempotency_key_unique UNIQUE (idempotency_key)
);

CREATE INDEX IF NOT EXISTS agenteval_jobs_queue_order
    ON agenteval_jobs (created_at ASC, id ASC) WHERE status = 'QUEUED';
CREATE INDEX IF NOT EXISTS agenteval_jobs_status_created
    ON agenteval_jobs (status, created_at DESC, id DESC);