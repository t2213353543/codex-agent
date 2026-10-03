package server

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type importProbeCaptureTx struct {
	pgx.Tx
	sql  string
	args []any
	err  error
}

type importProbeErrorRow struct{ err error }

func (r importProbeErrorRow) Scan(...any) error { return r.err }

func (tx *importProbeCaptureTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	tx.sql, tx.args = sql, args
	return importProbeErrorRow{tx.err}
}

func (tx *importProbeCaptureTx) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.sql, tx.args = sql, args
	return nil, tx.err
}

// Exercise the real probe methods without a database: an execution-mode option
// must precede the same bound, typed arrays for every batch size, not just 10k.
func TestImportBulkProbeUsesUncachedBoundParameters(t *testing.T) {
	for _, size := range []int{2, 100, 1000, 10000} {
		for _, duplicate := range []bool{false, true} {
			t.Run(fmt.Sprintf("size_%d_duplicate_%t", size, duplicate), func(t *testing.T) {
				parsed := make([]Account, size)
				report := ImportReport{Results: make([]ImportResult, size)}
				names, fingerprints := make([]string, size), make([][]byte, size)
				for i := range parsed {
					parsed[i] = Account{Name: fmt.Sprintf("bound_'comma,brace{}slash\\中文_%d", i), AccessToken: fmt.Sprintf("token_%d", i)}
					names[i], fingerprints[i] = parsed[i].Name, tokenFingerprint(parsed[i].AccessToken)
				}
				want := errors.New("stop after bound probe")
				tx := &importProbeCaptureTx{err: want}
				var handled bool
				var err error
				if duplicate {
					handled, err = (Store{}).pgImportUnchangedAccounts(t.Context(), tx, parsed, &report)
				} else {
					handled, err = (Store{}).pgImportNewAccounts(t.Context(), tx, parsed, &report)
				}
				if handled || !errors.Is(err, want) || report.Added != 0 || report.Duplicate != 0 {
					t.Fatalf("probe failure changed outcome: handled=%t report=%+v err=%v", handled, report, err)
				}
				wantArgs := []any{pgx.QueryExecModeExec, names, fingerprints}
				if duplicate {
					wantArgs = []any{pgx.QueryExecModeExec, fingerprints}
				}
				if !reflect.DeepEqual(tx.args, wantArgs) {
					t.Fatal("probe did not use uncached execution with the original bound arrays")
				}
				if !strings.Contains(tx.sql, "=ANY($1::") || strings.Contains(tx.sql, names[0]) || strings.Contains(tx.sql, parsed[0].AccessToken) {
					t.Fatal("probe SQL lost parameter binding")
				}
			})
		}
	}
}

// ExtendedQueryBuilder with a nil description is pgx's actual Exec-mode
// encoding path. This proves [][]byte and text[] remain supported, including
// binary zero bytes and names requiring array-text escaping, without PostgreSQL.
func TestImportBulkExecParameterEncodingRoundTrip(t *testing.T) {
	names := []string{"normal", "quote'\"", "comma,braces{}", "back\\slash", "中文", "NULL", ""}
	fingerprints := make([][]byte, 10000)
	for i := range fingerprints {
		fingerprints[i] = tokenFingerprint(fmt.Sprintf("encoding-%d", i))
	}
	mapping := pgtype.NewMap()
	var builder pgx.ExtendedQueryBuilder
	if err := builder.Build(mapping, nil, []any{names, fingerprints}); err != nil {
		t.Fatalf("Exec-mode encoding: %v", err)
	}
	if !reflect.DeepEqual(builder.ParamFormats, []int16{pgtype.TextFormatCode, pgtype.TextFormatCode}) {
		t.Fatalf("unexpected protocol formats: %v", builder.ParamFormats)
	}
	var decodedNames []string
	var decodedFingerprints [][]byte
	if err := mapping.Scan(pgtype.TextArrayOID, pgtype.TextFormatCode, builder.ParamValues[0], &decodedNames); err != nil {
		t.Fatal(err)
	}
	if err := mapping.Scan(pgtype.ByteaArrayOID, pgtype.TextFormatCode, builder.ParamValues[1], &decodedFingerprints); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, decodedNames) || !reflect.DeepEqual(fingerprints, decodedFingerprints) {
		t.Fatal("bound import array changed on the Exec-mode encoding path")
	}
}

func TestImportBulkCancellationBeforeProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	parsed, report, err := prepareImport(bulkImportFixtureItems(2, "cancel-probe"), AccountDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	for _, duplicate := range []bool{false, true} {
		tx := new(importProbeCaptureTx)
		var handled bool
		if duplicate {
			handled, err = (Store{}).pgImportUnchangedAccounts(ctx, tx, parsed, &report)
		} else {
			handled, err = (Store{}).pgImportNewAccounts(ctx, tx, parsed, &report)
		}
		if handled || !errors.Is(err, context.Canceled) || tx.sql != "" || report.Added != 0 || report.Duplicate != 0 {
			t.Fatalf("canceled probe touched the transaction: handled=%t err=%v sql=%q", handled, err, tx.sql)
		}
	}
}
