package rdsdata

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/statiolake/aws-mcp/internal/caller"
	"github.com/statiolake/aws-mcp/internal/fakeaws"
)

func TestMain(m *testing.M) {
	fakeaws.MaybeRun()
	os.Exit(m.Run())
}

type step struct {
	op   string
	body map[string]any
}

type fakeDataAPI struct {
	mu       sync.Mutex
	steps    []step
	execErr  error
	rollback error
}

func (f *fakeDataAPI) Call(ctx context.Context, req *caller.Request) (json.RawMessage, error) {
	var body map[string]any
	json.Unmarshal(req.Params, &body)
	f.mu.Lock()
	f.steps = append(f.steps, step{req.Operation, body})
	f.mu.Unlock()
	if req.Service != "rds-data" {
		return nil, &caller.APIError{Code: "Wrong", Message: req.Service}
	}
	switch req.Operation {
	case "BeginTransaction":
		return json.RawMessage(`{"transactionId":"tx-1"}`), nil
	case "RollbackTransaction":
		if f.rollback != nil {
			return nil, f.rollback
		}
		return json.RawMessage(`{"transactionStatus":"Rollback Complete"}`), nil
	case "ExecuteStatement":
		if body["sql"] == "SET TRANSACTION READ ONLY" {
			return json.RawMessage(`{}`), nil
		}
		if f.execErr != nil {
			return nil, f.execErr
		}
		return json.RawMessage(`{"columnMetadata":[{"name":"id","label":"id","typeName":"int4"},{"name":"name","label":"name","typeName":"text"}],` +
			`"formattedRecords":"[{\"id\":1,\"name\":\"a\"},{\"id\":2,\"name\":\"b\"}]","numberOfRecordsUpdated":0}`), nil
	}
	return nil, &caller.APIError{Code: "Unknown"}
}

var q = Query{ResourceARN: "arn:aws:rds:ap-northeast-1:1:cluster:c", SecretARN: "arn:aws:secretsmanager:ap-northeast-1:1:secret:ro",
	Database: "app", SQL: "SELECT id, name FROM users WHERE id > :min", Parameters: json.RawMessage(`{"min": 0, "flag": true, "s": "x", "n": null, "f": 1.5}`)}

func TestRunRollsBack(t *testing.T) {
	f := &fakeDataAPI{}
	res, err := (&Runner{Caller: f, SetReadOnlyTxn: true}).Run(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if res.NumberOfRecords != 2 || res.Columns[1].Name != "name" || !res.RolledBack || string(res.Records[0]) != `{"id":1,"name":"a"}` {
		t.Fatalf("res = %+v", res)
	}
	ops := []string{}
	for _, s := range f.steps {
		ops = append(ops, s.op)
	}
	if strings.Join(ops, ",") != "BeginTransaction,ExecuteStatement,ExecuteStatement,RollbackTransaction" {
		t.Fatalf("ops = %v", ops)
	}
	if f.steps[1].body["sql"] != "SET TRANSACTION READ ONLY" {
		t.Fatalf("no read-only txn: %v", f.steps[1].body)
	}
	ex := f.steps[2].body
	if ex["transactionId"] != "tx-1" || ex["includeResultMetadata"] != true || ex["formatRecordsAs"] != "JSON" || ex["secretArn"] != q.SecretARN {
		t.Fatalf("execute body = %v", ex)
	}
	b, _ := json.Marshal(ex["parameters"])
	want := `[{"name":"f","value":{"doubleValue":1.5}},{"name":"flag","value":{"booleanValue":true}},{"name":"min","value":{"longValue":0}},{"name":"n","value":{"isNull":true}},{"name":"s","value":{"stringValue":"x"}}]`
	if string(b) != want {
		t.Fatalf("parameters = %s", b)
	}
	if f.steps[3].body["transactionId"] != "tx-1" {
		t.Fatalf("rollback body = %v", f.steps[3].body)
	}
}

func TestRunRollsBackOnError(t *testing.T) {
	f := &fakeDataAPI{execErr: &caller.APIError{Code: "BadRequestException", Message: "relation \"users\" does not exist"}}
	ctx, cancel := context.WithCancel(context.Background())
	res, err := (&Runner{Caller: f}).Run(ctx, q)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "BadRequestException") {
		t.Fatalf("err = %v", err)
	}
	if res == nil || !res.RolledBack || f.steps[len(f.steps)-1].op != "RollbackTransaction" {
		t.Fatalf("not rolled back: %+v %v", res, f.steps)
	}
}

func TestRunRollbackAfterCancel(t *testing.T) {
	f := &fakeDataAPI{}
	ctx, cancel := context.WithCancel(context.Background())
	c := caller.Func(func(cctx context.Context, req *caller.Request) (json.RawMessage, error) {
		if req.Operation == "ExecuteStatement" {
			cancel()
			return nil, &caller.APIError{Code: caller.CodeTimeout, Message: "cancelled"}
		}
		if cctx.Err() != nil {
			return nil, cctx.Err()
		}
		return f.Call(cctx, req)
	})
	res, err := (&Runner{Caller: c}).Run(ctx, q)
	if err == nil || !res.RolledBack {
		t.Fatalf("err=%v res=%+v", err, res)
	}
}

func TestRunRefusesWrites(t *testing.T) {
	f := &fakeDataAPI{}
	w := q
	w.SQL = "DELETE FROM users"
	if _, err := (&Runner{Caller: f}).Run(context.Background(), w); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v", err)
	}
	if len(f.steps) != 0 {
		t.Fatalf("calls made for refused SQL: %v", f.steps)
	}
}

func TestRunRawParameters(t *testing.T) {
	f := &fakeDataAPI{}
	r := q
	r.Parameters = json.RawMessage(`[{"name":"d","value":{"stringValue":"2024-01-01"},"typeHint":"DATE"}]`)
	if _, err := (&Runner{Caller: f}).Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(f.steps[1].body["parameters"])
	if !strings.Contains(string(b), `"typeHint":"DATE"`) {
		t.Fatalf("parameters = %s", b)
	}
	r.Parameters = json.RawMessage(`{"x": [1]}`)
	if _, err := (&Runner{Caller: f}).Run(context.Background(), r); err == nil {
		t.Fatal("list value accepted")
	}
}

func TestRunWithFakeCLI(t *testing.T) {
	f := fakeaws.New(t, []fakeaws.Rule{
		{Service: "rds-data", Operation: "begin-transaction", Stdout: map[string]any{"transactionId": "tx-9"}},
		{Service: "rds-data", Operation: "execute-statement", Match: map[string]any{"transactionId": "tx-9", "formatRecordsAs": "JSON", "includeResultMetadata": true},
			Stdout: map[string]any{"columnMetadata": []any{map[string]any{"label": "n", "typeName": "int8"}}, "formattedRecords": `[{"n":42}]`}},
		{Service: "rds-data", Operation: "rollback-transaction", Match: map[string]any{"transactionId": "tx-9"},
			Stdout: map[string]any{"transactionStatus": "Rollback Complete"}},
	})
	cli := caller.NewCLI(caller.CLI{Path: f.Path, Env: f.Env})
	sq := q
	sq.SQL = "SELECT count(*) AS n FROM users"
	sq.Parameters = nil
	sq.Region = "ap-northeast-1"
	res, err := (&Runner{Caller: cli}).Run(context.Background(), sq)
	if err != nil {
		t.Fatal(err)
	}
	if !res.RolledBack || string(res.Records[0]) != `{"n":42}` {
		t.Fatalf("res = %+v", res)
	}
	inv := f.Invocations()
	if len(inv) != 3 || inv[2].Operation != "rollback-transaction" || inv[1].Region != "ap-northeast-1" {
		t.Fatalf("invocations = %+v", inv)
	}
}
