-- Pending provider flows, such as an OIDC sign-in or a GitHub authorization,
-- between their start and their callback, so the callback may reach any
-- gateway. kind's group, before its first '/', shares one bound of live flows;
-- key is the digest of the flow's state; subject is whose flow it is. body holds
-- the flow's own JSON, with any provider token sealed by its owner.
CREATE TABLE pending_flows (
    org text NOT NULL,
    kind text COLLATE "C" NOT NULL,
    key text COLLATE "C" NOT NULL,
    subject text COLLATE "C" NOT NULL,
    expires timestamptz NOT NULL,
    body bytea NOT NULL,
    PRIMARY KEY (org, kind, key)
);
CREATE INDEX pending_flows_subject ON pending_flows (org, subject);

-- One-minute request windows for sign-in and OAuth limits, shared by every
-- gateway: count requests charged since started.
CREATE TABLE rate_windows (
    org text NOT NULL,
    key text COLLATE "C" NOT NULL,
    started timestamptz NOT NULL,
    count integer NOT NULL CHECK (count >= 0),
    PRIMARY KEY (org, key)
);
