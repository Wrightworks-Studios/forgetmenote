-- Daily totals of Jev's paid attempts that left the ledger. A full ledger turns
-- its oldest settled attempts into these totals and removes them, so usage
-- stays accounted by day, actor, notebook, feature and model while the ledger
-- keeps room for new attempts. Reserved and unknown attempts never move here.
-- Older binaries ignore this table and read the ledger as before.
CREATE TABLE jev_usage_totals (
    org text NOT NULL,
    day date NOT NULL,
    feature text NOT NULL,
    actor text NOT NULL,
    notebook text NOT NULL,
    requested_model text NOT NULL,
    returned_model text NOT NULL,
    attempts bigint NOT NULL,
    input_tokens bigint NOT NULL,
    output_tokens bigint NOT NULL,
    estimate bigint NOT NULL,
    PRIMARY KEY (org, day, feature, actor, notebook, requested_model, returned_model)
);
