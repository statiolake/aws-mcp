package rdsdata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/statiolake/aws-mcp/internal/caller"
)

// Query is one rds_query request.
type Query struct {
	ResourceARN string
	SecretARN   string
	Database    string
	SQL         string
	// Parameters is either an object {"name": value} (values converted to
	// Data API fields by JSON type) or a raw Data API SqlParameter list.
	Parameters json.RawMessage
	Region     string
	Profile    string
}

// Column describes one result column.
type Column struct {
	Name     string `json:"name"`
	TypeName string `json:"typeName,omitempty"`
}

// Result is the outcome of a query.
type Result struct {
	Columns         []Column          `json:"columns"`
	Records         []json.RawMessage `json:"records"`
	NumberOfRecords int               `json:"numberOfRecords"`
	RolledBack      bool              `json:"rolledBack"`
	RollbackError   string            `json:"rollbackError,omitempty"`
}

// Runner executes queries. rds_query has its own protections (CheckReadOnly
// and an always rolled-back transaction).
type Runner struct {
	Caller caller.Caller
	// SetReadOnlyTxn issues SET TRANSACTION READ ONLY after BEGIN (honoured
	// by PostgreSQL; MySQL rejects it mid-transaction, which is ignored).
	SetReadOnlyTxn bool
}

// Run checks the statement, then runs it inside a Data API transaction that
// is always rolled back.
func (r *Runner) Run(ctx context.Context, q Query) (*Result, error) {
	if err := CheckReadOnly(q.SQL); err != nil {
		return nil, err
	}
	if q.ResourceARN == "" || q.SecretARN == "" || q.Database == "" {
		return nil, fmt.Errorf("resource_arn, secret_arn and database are required")
	}
	params, err := convertParameters(q.Parameters)
	if err != nil {
		return nil, err
	}
	call := func(ctx context.Context, op string, body map[string]any) (json.RawMessage, error) {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		return r.Caller.Call(ctx, &caller.Request{Service: "rds-data", Operation: op, Params: b, Region: q.Region, Profile: q.Profile})
	}

	begin, err := call(ctx, "BeginTransaction", map[string]any{
		"resourceArn": q.ResourceARN, "secretArn": q.SecretARN, "database": q.Database,
	})
	if err != nil {
		return nil, err
	}
	var tx struct {
		TransactionID string `json:"transactionId"`
	}
	if err := json.Unmarshal(begin, &tx); err != nil || tx.TransactionID == "" {
		return nil, fmt.Errorf("BeginTransaction returned no transactionId: %s", begin)
	}

	res := &Result{}
	defer func() {
		// Roll back even if the caller's context is cancelled.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		_, rerr := call(rctx, "RollbackTransaction", map[string]any{
			"resourceArn": q.ResourceARN, "secretArn": q.SecretARN, "transactionId": tx.TransactionID,
		})
		if rerr != nil {
			res.RollbackError = rerr.Error()
		} else {
			res.RolledBack = true
		}
	}()

	if r.SetReadOnlyTxn {
		_, _ = call(ctx, "ExecuteStatement", map[string]any{
			"resourceArn": q.ResourceARN, "secretArn": q.SecretARN, "database": q.Database,
			"transactionId": tx.TransactionID, "sql": "SET TRANSACTION READ ONLY",
		})
	}

	body := map[string]any{
		"resourceArn": q.ResourceARN, "secretArn": q.SecretARN, "database": q.Database,
		"transactionId": tx.TransactionID, "sql": q.SQL,
		"includeResultMetadata": true, "formatRecordsAs": "JSON",
	}
	if len(params) > 0 {
		body["parameters"] = params
	}
	out, err := call(ctx, "ExecuteStatement", body)
	if err != nil {
		return res, err
	}
	var es struct {
		ColumnMetadata []struct {
			Name     string `json:"name"`
			Label    string `json:"label"`
			TypeName string `json:"typeName"`
		} `json:"columnMetadata"`
		FormattedRecords string `json:"formattedRecords"`
	}
	if err := json.Unmarshal(out, &es); err != nil {
		return res, fmt.Errorf("unexpected ExecuteStatement response: %v", err)
	}
	for _, c := range es.ColumnMetadata {
		name := c.Label
		if name == "" {
			name = c.Name
		}
		res.Columns = append(res.Columns, Column{Name: name, TypeName: c.TypeName})
	}
	res.Records = []json.RawMessage{}
	if es.FormattedRecords != "" {
		if err := json.Unmarshal([]byte(es.FormattedRecords), &res.Records); err != nil {
			return res, fmt.Errorf("cannot parse formattedRecords: %v", err)
		}
	}
	res.NumberOfRecords = len(res.Records)
	return res, nil
}

// convertParameters accepts {"id": 1, "name": "x"} or a raw SqlParameter
// list and returns the Data API form.
func convertParameters(raw json.RawMessage) ([]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '[' {
		var list []any
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("parameters: %v", err)
		}
		return list, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parameters must be an object {name: value} or a Data API parameter list: %v", err)
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []any
	for _, name := range names {
		field, err := toField(m[name])
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %v", name, err)
		}
		out = append(out, map[string]any{"name": name, "value": field})
	}
	return out, nil
}

var fieldKeys = map[string]bool{"stringValue": true, "longValue": true, "doubleValue": true,
	"booleanValue": true, "isNull": true, "blobValue": true, "arrayValue": true}

func toField(v any) (map[string]any, error) {
	switch x := v.(type) {
	case nil:
		return map[string]any{"isNull": true}, nil
	case bool:
		return map[string]any{"booleanValue": x}, nil
	case string:
		return map[string]any{"stringValue": x}, nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return map[string]any{"longValue": i}, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, err
		}
		return map[string]any{"doubleValue": f}, nil
	case map[string]any:
		if len(x) == 1 {
			for k := range x {
				if fieldKeys[k] {
					return x, nil
				}
			}
		}
		return nil, fmt.Errorf("object values must be a single Data API field such as {\"stringValue\": ...}")
	}
	return nil, fmt.Errorf("unsupported value type %T", v)
}
