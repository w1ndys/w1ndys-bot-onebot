// 📌 影响范围：使用内存事务 fake 验证违禁词 CRUD、乐观锁与审计原子性；不访问外部数据库。
package forbiddenmessagemonitor

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/w1ndys-bot/internal/management"
)

type lexiconTestDatabase struct {
	tx       *lexiconTestTx
	beginErr error
}

func (d *lexiconTestDatabase) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return d.tx, nil
}

func (*lexiconTestDatabase) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected Query")
}

func (*lexiconTestDatabase) QueryRow(context.Context, string, ...any) pgx.Row {
	return lexiconTestRow{err: errors.New("unexpected QueryRow")}
}

type lexiconExecResult struct {
	tag pgconn.CommandTag
	err error
}

type lexiconTestTx struct {
	rows        []pgx.Row
	querySQL    []string
	queryArgs   [][]any
	execResults []lexiconExecResult
	execSQL     []string
	execArgs    [][]any
	commitErr   error
	committed   bool
	rolledBack  bool
}

func (t *lexiconTestTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	t.querySQL = append(t.querySQL, sql)
	t.queryArgs = append(t.queryArgs, args)
	if len(t.rows) == 0 {
		return lexiconTestRow{err: errors.New("unexpected QueryRow")}
	}
	row := t.rows[0]
	t.rows = t.rows[1:]
	return row
}

func (t *lexiconTestTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.execSQL = append(t.execSQL, sql)
	t.execArgs = append(t.execArgs, args)
	if len(t.execResults) == 0 {
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}
	result := t.execResults[0]
	t.execResults = t.execResults[1:]
	return result.tag, result.err
}

func (t *lexiconTestTx) Commit(context.Context) error {
	if t.commitErr != nil {
		return t.commitErr
	}
	t.committed = true
	return nil
}

func (t *lexiconTestTx) Rollback(context.Context) error {
	if !t.committed {
		t.rolledBack = true
	}
	return nil
}

func (t *lexiconTestTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (*lexiconTestTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unused")
}
func (*lexiconTestTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("unused")
}
func (*lexiconTestTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (*lexiconTestTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (*lexiconTestTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, errors.New("unused")
}
func (*lexiconTestTx) Conn() *pgx.Conn { return nil }

type lexiconTestRow struct {
	values []any
	err    error
}

func (r lexiconTestRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("scan destination count mismatch")
	}
	for index, value := range r.values {
		reflect.ValueOf(dest[index]).Elem().Set(reflect.ValueOf(value))
	}
	return nil
}

func lexiconTestActor() management.Actor {
	return management.Actor{ID: "admin-1", Role: "super_admin", Channel: management.ChannelWebUI, RequestID: "request-1"}
}

func decodeAuditTerm(t *testing.T, value any) Term {
	t.Helper()
	raw, ok := value.([]byte)
	if !ok || len(raw) == 0 {
		t.Fatalf("audit value = %#v", value)
	}
	var term Term
	if err := json.Unmarshal(raw, &term); err != nil {
		t.Fatal(err)
	}
	return term
}

func TestLexiconRepositoryCreateTermAuditsAndCommits(t *testing.T) {
	updatedAt := time.Date(2026, 7, 29, 8, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	tx := &lexiconTestTx{rows: []pgx.Row{lexiconTestRow{values: []any{int64(7), "加微信", 25.5, int64(1), updatedAt}}}}
	repository := &lexiconRepository{database: &lexiconTestDatabase{tx: tx}}

	term, err := repository.CreateTerm(context.Background(), lexiconTestActor(), TermInput{Text: "加微信", Weight: 25.5})
	if err != nil {
		t.Fatal(err)
	}
	if !tx.committed || tx.rolledBack || term.ID != 7 || term.Weight != 25.5 || term.UpdatedAt.Location() != time.UTC {
		t.Fatalf("term=%+v tx=%+v", term, tx)
	}
	if len(tx.execSQL) != 1 || !strings.Contains(tx.execSQL[0], "INSERT INTO admin_audit_logs") {
		t.Fatalf("audit SQL = %v", tx.execSQL)
	}
	if raw, ok := tx.execArgs[0][6].([]byte); !ok || len(raw) != 0 {
		t.Fatalf("audit before = %#v", tx.execArgs[0][6])
	}
	after := decodeAuditTerm(t, tx.execArgs[0][7])
	if after.ID != 7 || after.Text != "加微信" || after.Weight != 25.5 || after.Version != 1 {
		t.Fatalf("audit after = %+v", after)
	}
}

func TestLexiconRepositoryUpdateTermUsesCASAndFullAudit(t *testing.T) {
	beforeTime := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	afterTime := beforeTime.Add(time.Hour)
	tx := &lexiconTestTx{rows: []pgx.Row{
		lexiconTestRow{values: []any{int64(7), "加微信", 25.5, int64(3), beforeTime}},
		lexiconTestRow{values: []any{int64(7), "加好友", 125.75, int64(4), afterTime}},
	}}
	repository := &lexiconRepository{database: &lexiconTestDatabase{tx: tx}}

	term, err := repository.UpdateTerm(context.Background(), lexiconTestActor(), 7, 3, TermInput{Text: "加好友", Weight: 125.75})
	if err != nil {
		t.Fatal(err)
	}
	if !tx.committed || len(tx.querySQL) != 2 || !strings.Contains(tx.querySQL[0], "FOR UPDATE") {
		t.Fatalf("queries=%v committed=%v", tx.querySQL, tx.committed)
	}
	if !reflect.DeepEqual(tx.queryArgs[1], []any{int64(7), int64(3), "加好友", 125.75}) {
		t.Fatalf("update args = %#v", tx.queryArgs[1])
	}
	before := decodeAuditTerm(t, tx.execArgs[0][6])
	after := decodeAuditTerm(t, tx.execArgs[0][7])
	if before.Text != "加微信" || before.Weight != 25.5 || before.Version != 3 || after.Text != "加好友" || after.Weight != 125.75 || after.Version != 4 || term.Version != 4 {
		t.Fatalf("before=%+v after=%+v term=%+v", before, after, term)
	}
}

func TestLexiconRepositoryUpdateTermRejectsStaleVersion(t *testing.T) {
	tx := &lexiconTestTx{rows: []pgx.Row{lexiconTestRow{values: []any{int64(7), "加微信", 25.5, int64(4), time.Now()}}}}
	repository := &lexiconRepository{database: &lexiconTestDatabase{tx: tx}}

	_, err := repository.UpdateTerm(context.Background(), lexiconTestActor(), 7, 3, TermInput{Text: "加好友", Weight: 30})
	if !errors.Is(err, ErrLexiconConflict) || tx.committed || !tx.rolledBack || len(tx.execSQL) != 0 || len(tx.querySQL) != 1 {
		t.Fatalf("error=%v tx=%+v", err, tx)
	}
}

func TestLexiconRepositoryDeleteTermAuditsFullBeforeImage(t *testing.T) {
	updatedAt := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	tx := &lexiconTestTx{
		rows: []pgx.Row{lexiconTestRow{values: []any{int64(7), "加微信", 125.75, int64(4), updatedAt}}},
		execResults: []lexiconExecResult{
			{tag: pgconn.NewCommandTag("DELETE 1")},
			{tag: pgconn.NewCommandTag("INSERT 0 1")},
		},
	}
	repository := &lexiconRepository{database: &lexiconTestDatabase{tx: tx}}

	if err := repository.DeleteTerm(context.Background(), lexiconTestActor(), 7, 4); err != nil {
		t.Fatal(err)
	}
	if !tx.committed || tx.rolledBack || len(tx.querySQL) != 1 || !strings.Contains(tx.querySQL[0], "SELECT id,text,weight,version,updated_at") || !strings.Contains(tx.querySQL[0], "FOR UPDATE") {
		t.Fatalf("queries=%v tx=%+v", tx.querySQL, tx)
	}
	if len(tx.execSQL) != 2 || !strings.Contains(tx.execSQL[0], "DELETE FROM forbidden_monitor_terms") || !strings.Contains(tx.execSQL[1], "INSERT INTO admin_audit_logs") {
		t.Fatalf("exec SQL = %v", tx.execSQL)
	}
	before := decodeAuditTerm(t, tx.execArgs[1][6])
	if before.ID != 7 || before.Text != "加微信" || before.Weight != 125.75 || before.Version != 4 || !before.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("audit before = %+v", before)
	}
	if raw, ok := tx.execArgs[1][7].([]byte); !ok || len(raw) != 0 {
		t.Fatalf("audit after = %#v", tx.execArgs[1][7])
	}
}

func TestLexiconRepositoryDeleteTermRejectsStaleVersion(t *testing.T) {
	tx := &lexiconTestTx{rows: []pgx.Row{lexiconTestRow{values: []any{int64(7), "加微信", 25.0, int64(4), time.Now()}}}}
	repository := &lexiconRepository{database: &lexiconTestDatabase{tx: tx}}

	err := repository.DeleteTerm(context.Background(), lexiconTestActor(), 7, 3)
	if !errors.Is(err, ErrLexiconConflict) || tx.committed || !tx.rolledBack || len(tx.execSQL) != 0 {
		t.Fatalf("error=%v tx=%+v", err, tx)
	}
}

func TestLexiconRepositoryRollsBackDeleteWhenAuditFails(t *testing.T) {
	auditFailure := errors.New("audit failed")
	tx := &lexiconTestTx{
		rows: []pgx.Row{lexiconTestRow{values: []any{int64(7), "加微信", 25.0, int64(4), time.Now()}}},
		execResults: []lexiconExecResult{
			{tag: pgconn.NewCommandTag("DELETE 1")},
			{err: auditFailure},
		},
	}
	repository := &lexiconRepository{database: &lexiconTestDatabase{tx: tx}}

	err := repository.DeleteTerm(context.Background(), lexiconTestActor(), 7, 4)
	if !errors.Is(err, auditFailure) || tx.committed || !tx.rolledBack || len(tx.execSQL) != 2 {
		t.Fatalf("error=%v tx=%+v", err, tx)
	}
}

func TestLexiconRepositoryRollsBackCreateWhenAuditFails(t *testing.T) {
	auditFailure := errors.New("audit failed")
	tx := &lexiconTestTx{
		rows:        []pgx.Row{lexiconTestRow{values: []any{int64(7), "加微信", 25.0, int64(1), time.Now()}}},
		execResults: []lexiconExecResult{{err: auditFailure}},
	}
	repository := &lexiconRepository{database: &lexiconTestDatabase{tx: tx}}

	_, err := repository.CreateTerm(context.Background(), lexiconTestActor(), TermInput{Text: "加微信", Weight: 25})
	if !errors.Is(err, auditFailure) || tx.committed || !tx.rolledBack {
		t.Fatalf("error=%v tx=%+v", err, tx)
	}
}
