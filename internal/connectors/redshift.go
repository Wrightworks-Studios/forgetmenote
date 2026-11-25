//go:build !fmn_omit_redshift

package connectors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/redshiftdata"
	"github.com/aws/aws-sdk-go-v2/service/redshiftdata/types"
)

var redshiftSQLState = regexp.MustCompile(`\b(42[0-9A-Z]{3}|22[0-9A-Z]{3})\b`)
var redshiftSQLFailure = regexp.MustCompile(`(?im)^ERROR: (syntax error at or near|column .+ does not exist|relation .+ does not exist|schema .+ does not exist|operator does not exist|function .+ does not exist|invalid input syntax|division by zero)`)

// redshiftClient calls a redshift connection's Redshift Data API. A build with
// the tag fmn_omit_redshift leaves Redshift connections out (redshift_omitted.go).
type redshiftClient = *redshiftdata.Client

type responseBoundTransport struct {
	base  http.RoundTripper
	bytes int64
}

func (t responseBoundTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r, err := t.base.RoundTrip(req)
	if err == nil {
		r.Body = &limitedBody{ReadCloser: r.Body, left: t.bytes}
	}
	return r, err
}

func (r *runtime) configureRedshift() error {
	c := r.config.Redshift
	if c == nil || c.Region == "" || c.Database == "" || (c.Cluster == "") == (c.Workgroup == "") || c.DBUser != "" && (c.Workgroup != "" || c.SecretARN != "") || r.config.SecretEnv != "" || r.config.SecretFile != "" {
		return errors.New("redshift requires region, database, one cluster/workgroup and runtime IAM credentials")
	}
	if err := c.validatePolicy(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.Region), awsconfig.WithHTTPClient(r.client), awsconfig.WithRetryMaxAttempts(2))
	if err != nil {
		return errors.New("cannot configure AWS runtime credentials")
	}
	r.aws = cfg.Credentials
	r.redshift = redshiftdata.NewFromConfig(cfg, func(o *redshiftdata.Options) {
		// The wire encoding includes metadata and type wrappers beyond user data.
		o.HTTPClient = noRedirectClient(responseBoundTransport{r.client.Transport, int64(r.config.MaxBytes)*4 + 64<<10}, r.client.Timeout)
		if r.config.Endpoint != "" {
			o.BaseEndpoint = aws.String(r.config.Endpoint)
		}
	})
	return nil
}

func (r *runtime) redshiftCall(ctx context.Context, name string, input map[string]any) (any, bool, error) {
	c := r.config.Redshift
	sql, _ := input["sql"].(string)
	params, _ := input["params"].(map[string]any)
	if name == "schema" {
		if schema, _ := input["schema"].(string); schema != "" && len(c.Schemas) > 0 && !slices.Contains(c.Schemas, schema) {
			return nil, false, fmt.Errorf("%w: schema excluded by connection policy; allowed schemas: %s; omit schema to discover matching tables across these schemas", ErrQuery, strings.Join(c.Schemas, ", "))
		}
		sql = "SELECT table_schema,table_name,column_name,data_type,is_nullable FROM information_schema.columns WHERE table_schema NOT IN ('pg_catalog','information_schema')"
		params = map[string]any{}
		for _, p := range []struct{ key, column string }{{"schema", "table_schema"}, {"table", "table_name"}} {
			if v, _ := input[p.key].(string); v != "" {
				sql += " AND " + p.column + "=:" + p.key
				params[p.key] = v
			}
		}
		if search, _ := input["search"].(string); search != "" {
			sql += " AND (strpos(lower(table_name),lower(:search))>0 OR strpos(lower(column_name),lower(:search))>0)"
			params["search"] = search
		}
		for _, filter := range []struct {
			column, operator string
			names            []string
		}{{"table_schema", "IN", c.Schemas}, {"column_name", "NOT IN", c.DeniedColumns}} {
			if len(filter.names) == 0 {
				continue
			}
			keys := []string{}
			for i, value := range filter.names {
				key := fmt.Sprintf("policy_%s_%d", filter.column, i)
				keys = append(keys, ":"+key)
				params[key] = value
			}
			sql += " AND " + filter.column + " " + filter.operator + " (" + strings.Join(keys, ",") + ")"
		}
		if input["detail"] == "tables" {
			sql = strings.Replace(sql, "SELECT table_schema,table_name,column_name,data_type,is_nullable", "SELECT DISTINCT table_schema,table_name", 1)
			sql += " ORDER BY table_schema,table_name"
		} else {
			sql += " ORDER BY table_schema,table_name,ordinal_position"
		}
	}
	sql, tokens, err := readExpression(sql)
	if err != nil {
		return nil, false, err
	}
	if name == "query" {
		if err := c.checkQuery(tokens, nil); err != nil {
			return nil, false, err
		}
	}
	token := make([]byte, 16)
	rand.Read(token)
	req := &redshiftdata.ExecuteStatementInput{Database: aws.String(c.Database), Sql: aws.String(fmt.Sprintf("SELECT * FROM (\n%s\n) AS fmn_result LIMIT %d", sql, r.config.MaxRows+1)), ClientToken: aws.String(hex.EncodeToString(token))}
	if c.Cluster != "" {
		req.ClusterIdentifier = aws.String(c.Cluster)
	}
	if c.Workgroup != "" {
		req.WorkgroupName = aws.String(c.Workgroup)
	}
	if c.DBUser != "" {
		req.DbUser = aws.String(c.DBUser)
	}
	if c.SecretARN != "" {
		req.SecretArn = aws.String(c.SecretARN)
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`).MatchString(k) {
			return nil, false, ErrInvalid
		}
		v, ok := params[k].(string)
		if !ok || v == "" {
			return nil, false, ErrInvalid
		}
		req.Parameters = append(req.Parameters, types.SqlParameter{Name: aws.String(k), Value: aws.String(v)})
	}
	if err := checkAuthority(ctx); err != nil {
		return nil, false, err
	}
	execution, err := r.redshift.ExecuteStatement(ctx, req)
	if err != nil {
		return nil, false, redshiftError(ctx)
	}
	if aws.ToString(execution.Id) == "" {
		return nil, false, ErrUnavailable
	}
	finished := false
	defer func() {
		if !finished {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = r.redshift.CancelStatement(cancelCtx, &redshiftdata.CancelStatementInput{Id: execution.Id})
		}
	}()
	for !finished {
		status, e := r.redshift.DescribeStatement(ctx, &redshiftdata.DescribeStatementInput{Id: execution.Id})
		if e != nil {
			return nil, false, redshiftError(ctx)
		}
		switch status.Status {
		case types.StatusStringFinished:
			finished = true
		case types.StatusStringFailed, types.StatusStringAborted:
			finished = true
			// Only syntax/data SQLSTATE errors are repairable; permission and
			// infrastructure failures must not consume a model turn.
			if (redshiftSQLState.MatchString(aws.ToString(status.Error)) || redshiftSQLFailure.MatchString(aws.ToString(status.Error))) && !strings.Contains(aws.ToString(status.Error), "42501") {
				return nil, false, ErrQuery
			}
			return nil, false, errors.New("Redshift statement failed")
		default:
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	rows := [][]any{}
	columns := []map[string]string{}
	size := 0
	var next *string
	seen := map[string]bool{}
	for {
		page, e := r.redshift.GetStatementResult(ctx, &redshiftdata.GetStatementResultInput{Id: execution.Id, NextToken: next})
		if e != nil {
			return nil, false, redshiftError(ctx)
		}
		if len(columns) == 0 {
			for _, col := range page.ColumnMetadata {
				columns = append(columns, map[string]string{"name": aws.ToString(col.Name), "type": aws.ToString(col.TypeName)})
			}
		}
		for _, record := range page.Records {
			if len(rows) == r.config.MaxRows || c.MaxCells > 0 && (len(rows)+1)*len(record) > c.MaxCells {
				return map[string]any{"columns": columns, "rows": rows}, false, nil
			}
			row := make([]any, 0, len(record))
			for _, field := range record {
				var value any
				switch f := field.(type) {
				case *types.FieldMemberStringValue:
					value = f.Value
				case *types.FieldMemberLongValue:
					value = f.Value
				case *types.FieldMemberDoubleValue:
					value = f.Value
				case *types.FieldMemberBooleanValue:
					value = f.Value
				case *types.FieldMemberBlobValue:
					value = f.Value
				case *types.FieldMemberIsNull:
					if !f.Value {
						return nil, false, ErrUnavailable
					}
				default:
					return nil, false, ErrUnavailable
				}
				row = append(row, value)
			}
			b, _ := json.Marshal(row)
			size += len(b)
			if size > r.config.MaxBytes {
				return nil, false, errors.New("database response exceeds connection limit")
			}
			rows = append(rows, row)
		}
		next = page.NextToken
		if aws.ToString(next) == "" {
			return map[string]any{"columns": columns, "rows": rows}, true, nil
		}
		if seen[*next] {
			return nil, false, ErrUnavailable
		}
		seen[*next] = true
	}
}
func redshiftError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("Redshift Data API unavailable")
}
