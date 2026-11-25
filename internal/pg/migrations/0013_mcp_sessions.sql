-- Sessions of MCP clients older than protocol 2026-07-28, so any gateway serves
-- each request of a session and a deploy keeps its client connected. id is the
-- digest of the session ID and subject the credential the session is bound to.
-- initialize holds the client's initialize parameters, from which a gateway
-- recreates the session; subscriptions maps each subscribed resource to the
-- account and credential that subscribed it. A session idle past expires is
-- gone.
CREATE TABLE mcp_sessions (
    org text NOT NULL,
    id text COLLATE "C" NOT NULL,
    subject text COLLATE "C" NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    initialize bytea NOT NULL,
    subscriptions jsonb NOT NULL,
    expires timestamptz NOT NULL,
    PRIMARY KEY (org, id)
);
CREATE INDEX mcp_sessions_subject ON mcp_sessions (org, subject, expires);
