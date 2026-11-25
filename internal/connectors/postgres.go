package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

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

func (r *runtime) postgresCall(ctx context.Context, name string, input map[string]any, secret string) (any, bool, error) {
	u, _ := url.Parse(r.config.Endpoint)
	config, err := pgx.ParseConfig(r.config.Endpoint)
	if err != nil {
		return nil, false, ErrInvalid
	}
	// Pin the destination, identity and session settings even when libpq defaults
	// exist in the operator environment. Never borrow a password from pgpass.
	config.Host = u.Hostname()
	config.Port = 5432
	if u.Port() != "" {
		port, e := strconv.ParseUint(u.Port(), 10, 16)
		if e != nil || port == 0 {
			return nil, false, ErrInvalid
		}
		config.Port = uint16(port)
	}
	config.User = u.User.Username()
	config.Database = strings.TrimPrefix(u.Path, "/")
	config.Password = secret
	config.Fallbacks = nil
	config.ValidateConnect = nil
	config.MaxProtocolMessageBodyLen = r.config.MaxBytes + 64<<10
	config.RuntimeParams = map[string]string{"application_name": "forgetmenote", "default_transaction_read_only": "on", "statement_timeout": fmt.Sprint(r.config.TimeoutSeconds * 1000), "lock_timeout": "2000", "search_path": "pg_catalog,public"}
	if config.TLSConfig != nil {
		config.TLSConfig.Certificates = nil
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, false, errors.New("database connection failed")
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		conn.Close(c)
	}()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, false, databaseError(err)
	}
	defer func() {
		rollback, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	sql, _ := input["sql"].(string)
	params, _ := input["params"].([]any)
	if name == "schema" {
		sql = `SELECT table_schema,table_name,column_name,data_type,is_nullable FROM information_schema.columns WHERE table_schema NOT IN ('pg_catalog','information_schema') AND ($1::text='' OR table_schema=$1) AND ($2::text='' OR table_name=$2) ORDER BY table_schema,table_name,ordinal_position`
		schema, _ := input["schema"].(string)
		table, _ := input["table"].(string)
		params = []any{schema, table}
		if search, _ := input["search"].(string); search != "" {
			sql = strings.Replace(sql, " ORDER BY", " AND (strpos(lower(table_name),lower($3))>0 OR strpos(lower(column_name),lower($3))>0) ORDER BY", 1)
			params = append(params, search)
		}
		if input["detail"] == "tables" {
			sql = strings.Replace(sql, "SELECT table_schema,table_name,column_name,data_type,is_nullable", "SELECT DISTINCT table_schema,table_name", 1)
			sql = strings.TrimSuffix(sql, ",ordinal_position")
		}
	}
	// A subquery cannot be a transaction-control, DDL, COPY or multiple SQL
	// statement. PostgreSQL's read-only transaction rejects modifying CTEs too.
	// Use a SELECT-only database role as the independent database permission.
	sql = fmt.Sprintf("SELECT * FROM (\n%s\n) AS fmn_result LIMIT %d", strings.TrimSuffix(strings.TrimSpace(sql), ";"), r.config.MaxRows+1)
	statement, err := conn.Prepare(ctx, "fmn_read", sql)
	if err != nil {
		return nil, false, databaseError(err)
	}
	if err := checkAuthority(ctx); err != nil {
		return nil, false, err
	}
	rows, err := tx.Query(ctx, statement.Name, params...)
	if err != nil {
		return nil, false, databaseError(err)
	}
	defer rows.Close()
	type column struct {
		Name string `json:"name"`
		Type uint32 `json:"typeOid"`
	}
	columns := []column{}
	for _, f := range rows.FieldDescriptions() {
		columns = append(columns, column{f.Name, f.DataTypeOID})
	}
	values := [][]any{}
	size := 0
	complete := true
	for rows.Next() {
		if len(values) >= r.config.MaxRows {
			complete = false
			break
		}
		v, e := rows.Values()
		if e != nil {
			return nil, false, databaseError(e)
		}
		b, e := json.Marshal(v)
		if e != nil {
			return nil, false, errors.New("unsupported database value")
		}
		size += len(b)
		if size > r.config.MaxBytes {
			return nil, false, errors.New("database response exceeds connection limit")
		}
		values = append(values, v)
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, false, databaseError(rows.Err())
	}
	return map[string]any{"columns": columns, "rows": values}, complete, nil
}

func databaseError(err error) error {
	var p *pgconn.PgError
	if errors.As(err, &p) {
		if strings.HasPrefix(p.Code, "42") && p.Code != "42501" || strings.HasPrefix(p.Code, "22") {
			return fmt.Errorf("%w (SQLSTATE %s)", ErrQuery, p.Code)
		}
		return fmt.Errorf("database query rejected (SQLSTATE %s)", p.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return errors.New("database query failed")
}
