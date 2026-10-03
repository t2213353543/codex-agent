package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

type managedClassDiagnosticQueryer struct {
	sql  string
	args []any
}

type managedClassBindTrace struct {
	output string
	stop   context.CancelFunc
	err    error
}

func (trace *managedClassBindTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.HasPrefix(data.SQL, "WITH selection_guard") {
		return ctx
	}
	raw, err := json.Marshal(map[string]any{"sql": data.SQL, "args": data.Args, "executed": false})
	if err == nil {
		err = os.WriteFile(trace.output, raw, 0600)
	}
	trace.err = err
	trace.stop()
	return ctx
}

func (trace *managedClassBindTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Unlike the SQL-only generator this invokes the production decision tree
// with its normal q.statsQueryer=nil. pgx tracing captures the final binds and
// cancels the context before the statement is executed. Only a connection is
// established; no query or mutation is submitted by this diagnostic test.
func TestManagedClassProductionBindCapture(t *testing.T) {
	output := os.Getenv("MANAGED_CLASS_BIND_OUTPUT")
	if output == "" {
		t.Skip("explicit bind capture output required")
	}
	values, err := url.ParseQuery(os.Getenv("MANAGED_CLASS_DIAGNOSTIC_QUERY"))
	if err != nil {
		t.Fatal(err)
	}
	q, err := parseManagedLogQuery(values)
	if err != nil {
		t.Fatal(err)
	}
	q.SnapshotID = q.AsOf
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	trace := &managedClassBindTrace{output: output, stop: cancel}
	// A local protocol stub is enough: query tracing cancels before sending
	// SQL. This avoids needing production credentials to inspect production
	// code's exact branch/argument construction.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		backend := pgproto3.NewBackend(conn, conn)
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			return
		}
		backend.Send(&pgproto3.AuthenticationOk{})
		backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "18.4"})
		backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
		backend.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: 1})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		_ = backend.Flush()
		_, _ = io.Copy(io.Discard, conn)
	}()
	config, err := pgxpool.ParseConfig("postgres://fixture@" + listener.Addr().String() + "/fixture?sslmode=disable")
	if err != nil {
		t.Fatal("bind capture connection config invalid")
	}
	config.MaxConns, config.MinConns = 1, 0
	config.ConnConfig.Tracer = trace
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("bind capture pool unavailable")
	}
	defer pool.Close()
	_, _ = (&Metrics{db: &Database{Pool: pool}}).managedLogStats(ctx, q)
	if trace.err != nil {
		t.Fatal(trace.err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal("final production query was not captured")
	}
}

func (q *managedClassDiagnosticQueryer) Query(_ context.Context, statement string, args ...any) (pgx.Rows, error) {
	q.sql, q.args = statement, append([]any{}, args...)
	return nil, errors.New("diagnostic captured without executing")
}

// Emit the exact production SQL without opening a database, executing a
// statement, or seeding a fixture. Optional snapshot IDs only parameterize
// this diagnostic; this helper never creates or reads a production snapshot.
func TestManagedClassMinuteDiagnosticSQL(t *testing.T) {
	query := os.Getenv("MANAGED_CLASS_DIAGNOSTIC_QUERY")
	if query == "" {
		query = "error_category=rate_limit"
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	q, err := parseManagedLogQuery(values)
	if err != nil {
		t.Fatal(err)
	}
	q.SnapshotID = q.AsOf
	if _, _, eligible := managedUsageClassRollup(q); !eligible {
		t.Fatal("diagnostic requires a single category/severity rollup query")
	}
	now := time.Now().Unix()
	span := q.To - q.From
	if q.To == 0 {
		span = now - q.From
	}
	step := q.Step
	if step == 0 {
		step = 3600
		if q.From == 0 || span > 90*86400 {
			step = 2592000
		} else if span > 7*86400 {
			step = 86400
		}
	}
	sql, args := managedUsageRollupQuery(q, managedUsageStats{Step: step})
	captured := new(managedClassDiagnosticQueryer)
	rawQuery := q
	rawQuery.statsQueryer = captured
	_, _ = (&Metrics{db: &Database{}}).managedLogStats(t.Context(), rawQuery)
	if captured.sql == "" {
		t.Fatal("raw SQL was not captured")
	}
	adaptiveSQL, adaptiveArgs := managedUsageAdaptiveClassSQL(q, sql, args, captured.sql, captured.args)
	q.Step = step
	source, sourceArgs := managedUsageRollupSourceWithMaterialization(q, false)
	value := map[string]any{"query": query, "timezone": q.Timezone, "step": step, "sql": sql, "args": args,
		"raw_sql": captured.sql, "raw_args": captured.args, "adaptive_sql": adaptiveSQL, "adaptive_args": adaptiveArgs,
		"source_sql": source, "source_args": sourceArgs,
		"probe_suffixes": map[string]string{
			"class_minutes": " SELECT count(*) AS minutes,count(*) FILTER(WHERE matched>0 AND matched=total) AS uniform,count(*) FILTER(WHERE matched>0 AND matched<total) AS mixed FROM class_minutes",
			"unsafe":        " SELECT count(*) FROM unsafe",
			"raw_rows":      " SELECT count(*) FROM raw_rows",
			"raw_projected": " SELECT count(*),COALESCE(sum(octet_length(record::text)),0) FROM raw_projected",
			"source":        " SELECT count(*) FROM source",
		}, "database_connected": false}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("MANAGED_CLASS_DIAGNOSTIC_OUTPUT"); path != "" {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("CLASS_MINUTE_DIAGNOSTIC %s", raw)
}
