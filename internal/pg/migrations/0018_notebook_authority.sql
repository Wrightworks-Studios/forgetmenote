-- A notebook's authority handoff between ForgetMeNote and GitHub, which the
-- workspace layer kept in authority.json and authority-stage.json beside a Git
-- working tree: state is its JSON receipt, and stage, while a handoff to
-- ForgetMeNote is prepared, the validated snapshot that handoff installs.
-- version grows with every write, so two gateways never replace each other's
-- record unseen. Deleting the notebook deletes its record.
CREATE TABLE notebook_authority (
    org text NOT NULL,
    notebook text COLLATE "C" NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    state jsonb NOT NULL,
    stage bytea,
    updated timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org, notebook),
    FOREIGN KEY (org, notebook) REFERENCES notebook_heads ON DELETE CASCADE
);
