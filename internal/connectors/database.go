package connectors

import (
	"encoding/json"
	"errors"
)

// The operation contract that the PostgreSQL and Redshift connections share.
// Their adapters are optional (postgres.go and redshift.go); this contract,
// the SQL guard and the configuration stay, so a build without them still
// recognizes their configuration and refuses it by name.

const querySchema = `{"type":"object","properties":{"sql":{"type":"string","minLength":1,"maxLength":65536},"params":{"type":"array","maxItems":128,"items":{"type":["string","number","boolean","null"]}}},"required":["sql"],"additionalProperties":false}`
const databaseSchema = `{"type":"object","properties":{"schema":{"type":"string","maxLength":128},"table":{"type":"string","maxLength":128},"detail":{"type":"string","enum":["tables","columns"],"description":"Use tables for compact discovery; columns (the default) for a selected table's types."},"search":{"type":"string","maxLength":128,"description":"Case-insensitive literal substring of a table or column name."}},"additionalProperties":false}`

// Invalid SQL or data can be repaired against the configured acceptance checks.
// Authentication, connectivity and resource failures cannot be fixed by a model.
var ErrQuery = errors.New("database query rejected")

func postgresOperation(op *Operation) error {
	if !op.ReadOnly {
		return errors.New("database operations must be read-only")
	}
	switch op.Name {
	case "query":
		op.InputSchema = json.RawMessage(querySchema)
		op.Description = "Execute a parameterized SELECT or WITH query in a read-only transaction. Use $1 parameters. Results include column types and explicit completeness."
	case "schema":
		op.InputSchema = json.RawMessage(databaseSchema)
		op.Description = "Discover accessible database structure. Start with detail:tables and a focused search term; then request columns for an exact table. Omit schema if unknown. Avoid dumping every column in the database."
	default:
		return errors.New("database operations are query and schema")
	}
	return nil
}

func redshiftOperation(op *Operation) error {
	if err := postgresOperation(op); err != nil {
		return err
	}
	if op.Name == "query" {
		op.InputSchema = json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string","minLength":1,"maxLength":65536},"params":{"type":"object","maxProperties":128,"additionalProperties":{"type":"string","minLength":1,"maxLength":65536}}},"required":["sql"],"additionalProperties":false}`)
		op.Description = "Execute a bounded SELECT/WITH query using Redshift Data API. Bind values with :name and params {name: string}. Requires an independently read-only database identity."
	}
	return nil
}
