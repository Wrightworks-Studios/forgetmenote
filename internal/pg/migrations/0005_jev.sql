-- Jev's paid-call ledger, one row per reserved attempt. The primary key lets
-- an operation's attempt be reserved once across every gateway, and only the
-- ledger that reserved it, named by holder, settles it.
CREATE TABLE jev_usage (
    org text NOT NULL,
    id text NOT NULL,
    seq bigint GENERATED ALWAYS AS IDENTITY,
    holder text NOT NULL,
    record jsonb NOT NULL,
    PRIMARY KEY (org, id)
);

CREATE INDEX jev_usage_order ON jev_usage (org, seq);

-- Jev's private orchestration records by name: preferences, notebook
-- settings, jobs, results, notifications and citation checks.
CREATE TABLE jev_records (
    org text NOT NULL,
    name text COLLATE "C" NOT NULL,
    body jsonb NOT NULL,
    PRIMARY KEY (org, name)
);
