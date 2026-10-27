-- Controller lease and monotonic fencing epoch. The empty lease is available
-- immediately, and lease decisions use clock_timestamp() on the database server.
CREATE TABLE IF NOT EXISTS agenteval_lease (
    singleton SMALLINT PRIMARY KEY CHECK (singleton = 1),
    lease_owner TEXT NULL CHECK (lease_owner IS NULL OR lease_owner ~ '^[a-f0-9]{24}$'),
    epoch BIGINT NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    lease_until TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01 00:00:00+00'
);

INSERT INTO agenteval_lease (singleton, lease_owner, epoch, lease_until)
VALUES (1, NULL, 0, '1970-01-01 00:00:00+00')
ON CONFLICT (singleton) DO NOTHING;
