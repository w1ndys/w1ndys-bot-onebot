package plugin

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

type runtimeStateTestDatabase struct {
	execSQL   string
	execArgs  []any
	execErr   error
	execCalls int
	querySQL  string
	queryArgs []any
	queryErr  error
	rows      pgx.Rows
	rowSQL    string
	rowArgs   []any
	row       pgx.Row
	tx        *runtimeStateTestTx
	beginErr  error
	txOptions pgx.TxOptions
}

// runtimeStateTestTx 按调用顺序回放事务内的 QueryRow 结果并记录审计写入。
type runtimeStateTestTx struct {
	rows       []pgx.Row
	rowSQL     []string
	rowArgs    [][]any
	execSQL    string
	execArgs   []any
	execErr    error
	execCalls  int
	commitErr  error
	committed  bool
	rolledBack bool
	queryRows  pgx.Rows
	queryErr   error
	querySQL   string
	queryArgs  []any
}

func (d *runtimeStateTestDatabase) BeginTx(_ context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	d.txOptions = options
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	if d.tx == nil {
		d.tx = &runtimeStateTestTx{}
	}
	return d.tx, nil
}

func (t *runtimeStateTestTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	t.rowSQL = append(t.rowSQL, sql)
	t.rowArgs = append(t.rowArgs, args)
	if len(t.rows) == 0 {
		return runtimeStateTestRow{err: errors.New("unexpected QueryRow")}
	}
	row := t.rows[0]
	t.rows = t.rows[1:]
	return row
}

func (t *runtimeStateTestTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.execCalls++
	t.execSQL, t.execArgs = sql, args
	return pgconn.NewCommandTag("INSERT 0 1"), t.execErr
}

func (t *runtimeStateTestTx) Commit(context.Context) error {
	if t.commitErr != nil {
		return t.commitErr
	}
	t.committed = true
	return nil
}

func (t *runtimeStateTestTx) Rollback(context.Context) error {
	if !t.committed {
		t.rolledBack = true
	}
	return nil
}

func (t *runtimeStateTestTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (t *runtimeStateTestTx) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	t.querySQL, t.queryArgs = sql, args
	return t.queryRows, t.queryErr
}
func (t *runtimeStateTestTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("unused")
}
func (t *runtimeStateTestTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (t *runtimeStateTestTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (t *runtimeStateTestTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, errors.New("unused")
}
func (t *runtimeStateTestTx) Conn() *pgx.Conn { return nil }

func (d *runtimeStateTestDatabase) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	d.rowSQL = sql
	d.rowArgs = args
	return d.row
}

func (d *runtimeStateTestDatabase) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.execCalls++
	d.execSQL = sql
	d.execArgs = args
	return pgconn.NewCommandTag("INSERT 0 1"), d.execErr
}

func (d *runtimeStateTestDatabase) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	d.querySQL = sql
	d.queryArgs = args
	return d.rows, d.queryErr
}

type runtimeStateTestRows struct {
	values  [][]any
	index   int
	err     error
	scanErr error
	closed  bool
}

type runtimeStateTestRow struct {
	values []any
	err    error
}

func (r runtimeStateTestRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return assignRuntimeStateValues(dest, r.values)
}

func (r *runtimeStateTestRows) Close()                                       { r.closed = true }
func (r *runtimeStateTestRows) Err() error                                   { return r.err }
func (r *runtimeStateTestRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *runtimeStateTestRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *runtimeStateTestRows) Values() ([]any, error)                       { return nil, errors.New("unused") }
func (r *runtimeStateTestRows) RawValues() [][]byte                          { return nil }
func (r *runtimeStateTestRows) Conn() *pgx.Conn                              { return nil }

func (r *runtimeStateTestRows) Next() bool {
	if r.index >= len(r.values) {
		r.closed = true
		return false
	}
	r.index++
	return true
}

func (r *runtimeStateTestRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	return assignRuntimeStateValues(dest, r.values[r.index-1])
}

func assignRuntimeStateValues(dest []any, values []any) error {
	if len(dest) != len(values) {
		return errors.New("scan destination count mismatch")
	}
	for index, value := range values {
		target := reflect.ValueOf(dest[index]).Elem()
		if value == nil {
			target.SetZero()
			continue
		}
		if target.Kind() == reflect.Pointer {
			pointer := reflect.New(target.Type().Elem())
			pointer.Elem().Set(reflect.ValueOf(value))
			target.Set(pointer)
			continue
		}
		target.Set(reflect.ValueOf(value))
	}
	return nil
}

func TestRuntimeStateRepositorySyncCatalogDefaultsMissingPluginsOff(t *testing.T) {
	catalog, err := NewSpecCatalog([]PluginSpec{validPluginSpec("tools", "tools"), validPluginSpec("echo", "echo")})
	if err != nil {
		t.Fatal(err)
	}
	database := &runtimeStateTestDatabase{}
	repository := &PostgresRuntimeStateRepository{database: database}
	if err := repository.SyncCatalog(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	wantSQL := `INSERT INTO plugin_states(plugin_key,desired_enabled)
SELECT plugin_key,FALSE FROM unnest($1::text[]) AS plugin_key
ON CONFLICT (plugin_key) DO NOTHING`
	if database.execCalls != 1 || normalizeRuntimeStateSQL(database.execSQL) != normalizeRuntimeStateSQL(wantSQL) {
		t.Fatalf("Exec() calls=%d sql=%q", database.execCalls, database.execSQL)
	}
	keys, ok := database.execArgs[0].([]string)
	if !ok || !reflect.DeepEqual(keys, []string{"echo", "tools"}) {
		t.Fatalf("keys = %#v", database.execArgs)
	}
}

func TestRuntimeStateRepositorySyncEmptyCatalogSkipsDatabase(t *testing.T) {
	catalog, err := NewSpecCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	database := &runtimeStateTestDatabase{}
	repository := &PostgresRuntimeStateRepository{database: database}
	if err := repository.SyncCatalog(context.Background(), catalog); err != nil || database.execCalls != 0 {
		t.Fatalf("SyncCatalog() error=%v calls=%d", err, database.execCalls)
	}
}

func TestRuntimeStateRepositoryLoadsGroupedUTCSnapshot(t *testing.T) {
	zone := time.FixedZone("test", 8*60*60)
	pluginTime := time.Date(2026, 7, 25, 20, 0, 0, 0, zone)
	groupTime := pluginTime.Add(time.Minute)
	rows := &runtimeStateTestRows{values: [][]any{
		{"echo", false, int64(1), pluginTime, nil, nil, nil, nil},
		{"monitor", true, int64(3), pluginTime, int64(100), true, int64(2), groupTime},
		{"monitor", true, int64(3), pluginTime, int64(200), false, int64(4), groupTime},
	}}
	repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{rows: rows}}
	states, err := repository.LoadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0].PluginKey != "echo" || states[0].DesiredEnabled || len(states[0].Groups) != 0 {
		t.Fatalf("echo state = %+v", states)
	}
	monitor := states[1]
	if monitor.PluginKey != "monitor" || !monitor.DesiredEnabled || monitor.Version != 3 || len(monitor.Groups) != 2 {
		t.Fatalf("monitor state = %+v", monitor)
	}
	if monitor.UpdatedAt.Location() != time.UTC || monitor.Groups[0].UpdatedAt.Location() != time.UTC {
		t.Fatalf("timestamps not UTC: %+v", monitor)
	}
	if monitor.Groups[0].GroupID != 100 || !monitor.Groups[0].Enabled || monitor.Groups[1].GroupID != 200 || monitor.Groups[1].Enabled {
		t.Fatalf("groups = %+v", monitor.Groups)
	}
	if !rows.closed {
		t.Fatal("rows not closed")
	}
	database := repository.database.(*runtimeStateTestDatabase)
	wantQuery := `SELECT s.plugin_key,s.desired_enabled,s.version,s.updated_at,
       g.group_id,g.enabled,g.version,g.updated_at
FROM plugin_states s
LEFT JOIN plugin_group_states g ON g.plugin_key=s.plugin_key
ORDER BY s.plugin_key,g.group_id`
	if normalizeRuntimeStateSQL(database.querySQL) != normalizeRuntimeStateSQL(wantQuery) {
		t.Fatalf("Query() sql = %q", database.querySQL)
	}
}

func TestRuntimeStateRepositoryPropagatesDatabaseErrors(t *testing.T) {
	databaseFailure := errors.New("database failed")
	catalog, _ := NewSpecCatalog([]PluginSpec{validPluginSpec("echo", "echo")})
	repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{execErr: databaseFailure}}
	if err := repository.SyncCatalog(context.Background(), catalog); !errors.Is(err, databaseFailure) {
		t.Fatalf("SyncCatalog() error = %v", err)
	}
	repository = &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{queryErr: databaseFailure}}
	if _, err := repository.LoadSnapshot(context.Background()); !errors.Is(err, databaseFailure) {
		t.Fatalf("LoadSnapshot query error = %v", err)
	}
	rows := &runtimeStateTestRows{values: [][]any{{"echo"}}, scanErr: databaseFailure}
	repository = &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{rows: rows}}
	if _, err := repository.LoadSnapshot(context.Background()); !errors.Is(err, databaseFailure) {
		t.Fatalf("LoadSnapshot scan error = %v", err)
	}
	rows = &runtimeStateTestRows{err: databaseFailure}
	repository = &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{rows: rows}}
	if _, err := repository.LoadSnapshot(context.Background()); !errors.Is(err, databaseFailure) {
		t.Fatalf("LoadSnapshot rows error = %v", err)
	}
}

func TestRuntimeStateRepositoryRejectsMissingDependencies(t *testing.T) {
	if repository, err := NewPostgresRuntimeStateRepository(nil); repository != nil || err == nil {
		t.Fatalf("NewPostgresRuntimeStateRepository(nil) = %v,%v", repository, err)
	}
	var repository *PostgresRuntimeStateRepository
	if err := repository.SyncCatalog(context.Background(), nil); err == nil {
		t.Fatal("nil repository SyncCatalog succeeded")
	}
	if _, err := repository.LoadSnapshot(context.Background()); err == nil {
		t.Fatal("nil repository LoadSnapshot succeeded")
	}
	repository = &PostgresRuntimeStateRepository{}
	if err := repository.SyncCatalog(context.Background(), nil); err == nil {
		t.Fatal("repository without database SyncCatalog succeeded")
	}
	if _, err := repository.LoadSnapshot(context.Background()); err == nil {
		t.Fatal("repository without database LoadSnapshot succeeded")
	}
	repository = &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{}}
	if err := repository.SyncCatalog(context.Background(), nil); err == nil {
		t.Fatal("SyncCatalog with nil catalog succeeded")
	}
}

var runtimeStateTestActor = management.Actor{ID: "10001", Role: "super_admin", Channel: management.ChannelWebUI, RequestID: "req-1"}

func TestRuntimeStateRepositoryFindsSinglePluginState(t *testing.T) {
	groupTime := time.Date(2026, 7, 25, 12, 0, 0, 0, time.FixedZone("test", 8*60*60))
	rows := &runtimeStateTestRows{values: [][]any{
		{"monitor", true, int64(3), groupTime, int64(200), false, int64(4), groupTime},
		{"monitor", true, int64(3), groupTime, int64(100), true, int64(2), groupTime},
	}}
	database := &runtimeStateTestDatabase{rows: rows}
	repository := &PostgresRuntimeStateRepository{database: database}
	state, err := repository.FindState(context.Background(), "monitor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(normalizeRuntimeStateSQL(database.querySQL), "WHERE s.plugin_key=$1 ORDER BY g.group_id") {
		t.Fatalf("Query() sql = %q", database.querySQL)
	}
	if !reflect.DeepEqual(database.queryArgs, []any{"monitor"}) {
		t.Fatalf("Query() args = %#v", database.queryArgs)
	}
	if state.PluginKey != "monitor" || state.Version != 3 || len(state.Groups) != 2 || state.UpdatedAt.Location() != time.UTC {
		t.Fatalf("state = %+v", state)
	}
}

func TestRuntimeStateRepositoryFindStateRejectsUnknownAndInvalidKeys(t *testing.T) {
	repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{rows: &runtimeStateTestRows{}}}
	if _, err := repository.FindState(context.Background(), "missing"); !errors.Is(err, ErrRuntimeStateNotFound) {
		t.Fatalf("FindState(missing) error = %v", err)
	}
	if _, err := repository.FindState(context.Background(), "bad-key"); err == nil {
		t.Fatal("invalid plugin key accepted")
	}
}

func TestRuntimeStateRepositorySaveConfigRedactsAuditSecrets(t *testing.T) {
	updatedAt := time.Date(2026, 7, 27, 3, 0, 0, 0, time.UTC)
	transaction := &runtimeStateTestTx{rows: []pgx.Row{
		runtimeStateTestRow{values: []any{json.RawMessage(`{"mode":"old","token":"old-secret"}`), int64(2)}},
		runtimeStateTestRow{values: []any{"monitor", json.RawMessage(`{"mode":"new","token":"new-secret"}`), int64(3), updatedAt}},
	}}
	repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
	schema := ConfigSchema{Fields: []ConfigField{
		{Key: "mode", DisplayName: "模式", Type: FieldString},
		{Key: "token", DisplayName: "令牌", Type: FieldSecret},
	}}
	_, err := repository.SaveConfig(context.Background(), runtimeStateTestActor, "monitor", schema, json.RawMessage(`{"mode":"new","token":"new-secret"}`), 2)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := transaction.execArgs[6].([]byte)
	after, _ := transaction.execArgs[7].([]byte)
	for label, snapshot := range map[string][]byte{"before": before, "after": after} {
		if strings.Contains(string(snapshot), "secret") || strings.Contains(string(snapshot), `"token"`) {
			t.Fatalf("audit %s leaked secret: %s", label, snapshot)
		}
		if !strings.Contains(string(snapshot), `"mode"`) {
			t.Fatalf("audit %s lost public config: %s", label, snapshot)
		}
	}
	if !transaction.committed {
		t.Fatal("transaction not committed")
	}
}

func TestRuntimeStateRepositoryUpdatesDesiredEnabledWithCASAndAudit(t *testing.T) {
	zone := time.FixedZone("test", 8*60*60)
	updatedAt := time.Date(2026, 7, 25, 20, 0, 0, 0, zone)
	transaction := &runtimeStateTestTx{rows: []pgx.Row{
		runtimeStateTestRow{values: []any{false, int64(2)}},
		runtimeStateTestRow{values: []any{"echo", true, int64(3), updatedAt}},
	}}
	repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
	state, err := repository.UpdateDesiredEnabled(context.Background(), runtimeStateTestActor, "echo", true, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(transaction.rowSQL) != 2 || !strings.Contains(transaction.rowSQL[0], "FOR UPDATE") {
		t.Fatalf("transaction QueryRow sql = %#v", transaction.rowSQL)
	}
	wantSQL := `UPDATE plugin_states
SET desired_enabled=$2,version=version+1,updated_at=NOW()
WHERE plugin_key=$1 AND version=$3
RETURNING plugin_key,desired_enabled,version,updated_at`
	if normalizeRuntimeStateSQL(transaction.rowSQL[1]) != normalizeRuntimeStateSQL(wantSQL) || !reflect.DeepEqual(transaction.rowArgs[1], []any{"echo", true, int64(2)}) {
		t.Fatalf("QueryRow() sql=%q args=%#v", transaction.rowSQL[1], transaction.rowArgs[1])
	}
	if !strings.Contains(transaction.execSQL, "admin_audit_logs") {
		t.Fatalf("audit sql = %q", transaction.execSQL)
	}
	wantAudit := []any{"10001", "super_admin", management.ChannelWebUI, "plugin.runtime.global.update", "plugin_runtime_state", "echo"}
	if !reflect.DeepEqual(transaction.execArgs[:6], wantAudit) || transaction.execArgs[8] != "req-1" {
		t.Fatalf("audit args = %#v", transaction.execArgs)
	}
	if before, ok := transaction.execArgs[6].([]byte); !ok || !strings.Contains(string(before), `"version":2`) {
		t.Fatalf("audit before = %#v", transaction.execArgs[6])
	}
	if after, ok := transaction.execArgs[7].([]byte); !ok || !strings.Contains(string(after), `"version":3`) {
		t.Fatalf("audit after = %#v", transaction.execArgs[7])
	}
	if !transaction.committed {
		t.Fatal("transaction not committed")
	}
	if state.PluginKey != "echo" || !state.DesiredEnabled || state.Version != 3 || state.UpdatedAt.Location() != time.UTC || len(state.Groups) != 0 {
		t.Fatalf("state = %+v", state)
	}
}

func TestRuntimeStateRepositoryGlobalWriteRejectsStaleAndMissingRows(t *testing.T) {
	tests := []struct {
		name string
		lock pgx.Row
		want error
	}{
		{name: "stale", lock: runtimeStateTestRow{values: []any{false, int64(5)}}, want: ErrRuntimeStateConflict},
		{name: "missing", lock: runtimeStateTestRow{err: pgx.ErrNoRows}, want: ErrRuntimeStateNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transaction := &runtimeStateTestTx{rows: []pgx.Row{test.lock}}
			repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
			if _, err := repository.UpdateDesiredEnabled(context.Background(), runtimeStateTestActor, "echo", true, 2); !errors.Is(err, test.want) {
				t.Fatalf("UpdateDesiredEnabled() error = %v", err)
			}
			if transaction.execSQL != "" || transaction.committed || !transaction.rolledBack {
				t.Fatalf("transaction wrote audit or committed: %+v", transaction)
			}
		})
	}
}

func TestRuntimeStateRepositorySetsGroupWithInsertOrCASUpdate(t *testing.T) {
	updatedAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name            string
		expectedVersion int64
		lockRows        []pgx.Row
		wantSQL         string
		wantArgs        []any
		returnedVersion int64
		wantBefore      bool
	}{
		{
			name: "insert", expectedVersion: 0, returnedVersion: 1,
			wantSQL: `INSERT INTO plugin_group_states(plugin_key,group_id,enabled)
VALUES($1,$2,$3)
ON CONFLICT (plugin_key,group_id) DO NOTHING
RETURNING group_id,enabled,version,updated_at`,
			wantArgs: []any{"echo", int64(100), true},
		},
		{
			name: "update", expectedVersion: 2, returnedVersion: 3, wantBefore: true,
			lockRows: []pgx.Row{runtimeStateTestRow{values: []any{false, int64(2)}}},
			wantSQL: `UPDATE plugin_group_states
SET enabled=$3,version=version+1,updated_at=NOW()
WHERE plugin_key=$1 AND group_id=$2 AND version=$4
RETURNING group_id,enabled,version,updated_at`,
			wantArgs: []any{"echo", int64(100), true, int64(2)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows := append(append([]pgx.Row{}, test.lockRows...), runtimeStateTestRow{values: []any{int64(100), true, test.returnedVersion, updatedAt}})
			transaction := &runtimeStateTestTx{rows: rows}
			repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
			state, err := repository.SetGroupEnabled(context.Background(), runtimeStateTestActor, "echo", 100, true, test.expectedVersion)
			if err != nil {
				t.Fatal(err)
			}
			writeIndex := len(test.lockRows)
			if normalizeRuntimeStateSQL(transaction.rowSQL[writeIndex]) != normalizeRuntimeStateSQL(test.wantSQL) || !reflect.DeepEqual(transaction.rowArgs[writeIndex], test.wantArgs) {
				t.Fatalf("QueryRow() sql=%q args=%#v", transaction.rowSQL[writeIndex], transaction.rowArgs[writeIndex])
			}
			wantAudit := []any{"plugin.runtime.group.update", "plugin_runtime_group_state", "echo:100"}
			if !reflect.DeepEqual(transaction.execArgs[3:6], wantAudit) || !transaction.committed {
				t.Fatalf("audit args = %#v committed=%v", transaction.execArgs, transaction.committed)
			}
			// 新增记录没有可信旧值，审计 before 必须为空。
			before, _ := transaction.execArgs[6].([]byte)
			if (len(before) > 0) != test.wantBefore {
				t.Fatalf("audit before = %q", before)
			}
			if state.GroupID != 100 || !state.Enabled || state.Version != test.returnedVersion || state.UpdatedAt.Location() != time.UTC {
				t.Fatalf("state = %+v", state)
			}
		})
	}
}

func TestRuntimeStateRepositoryGroupWriteRejectsStaleAndMissingRows(t *testing.T) {
	tests := []struct {
		name string
		lock pgx.Row
		want error
	}{
		{name: "stale", lock: runtimeStateTestRow{values: []any{false, int64(5)}}, want: ErrRuntimeStateConflict},
		{name: "missing", lock: runtimeStateTestRow{err: pgx.ErrNoRows}, want: ErrRuntimeStateConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transaction := &runtimeStateTestTx{rows: []pgx.Row{test.lock}}
			repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
			if _, err := repository.SetGroupEnabled(context.Background(), runtimeStateTestActor, "echo", 100, true, 2); !errors.Is(err, test.want) {
				t.Fatalf("SetGroupEnabled() error = %v", err)
			}
			if transaction.execSQL != "" || transaction.committed {
				t.Fatalf("transaction wrote audit or committed: %+v", transaction)
			}
		})
	}
}

func TestRuntimeStateRepositoryWriteFailuresAndValidation(t *testing.T) {
	databaseFailure := errors.New("database failed")
	tests := []struct {
		name string
		row  pgx.Row
		want error
	}{
		{name: "conflict", row: runtimeStateTestRow{err: pgx.ErrNoRows}, want: ErrRuntimeStateConflict},
		{name: "database", row: runtimeStateTestRow{err: databaseFailure}, want: databaseFailure},
	}
	for _, test := range tests {
		t.Run("global "+test.name, func(t *testing.T) {
			transaction := &runtimeStateTestTx{rows: []pgx.Row{runtimeStateTestRow{values: []any{false, int64(1)}}, test.row}}
			repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
			if _, err := repository.UpdateDesiredEnabled(context.Background(), runtimeStateTestActor, "echo", true, 1); !errors.Is(err, test.want) {
				t.Fatalf("UpdateDesiredEnabled() error = %v", err)
			}
			if transaction.committed || !transaction.rolledBack {
				t.Fatalf("transaction committed=%v rolledBack=%v", transaction.committed, transaction.rolledBack)
			}
		})
		t.Run("group "+test.name, func(t *testing.T) {
			transaction := &runtimeStateTestTx{rows: []pgx.Row{test.row}}
			repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
			if _, err := repository.SetGroupEnabled(context.Background(), runtimeStateTestActor, "echo", 100, true, 0); !errors.Is(err, test.want) {
				t.Fatalf("SetGroupEnabled() error = %v", err)
			}
			if transaction.committed || !transaction.rolledBack {
				t.Fatalf("transaction committed=%v rolledBack=%v", transaction.committed, transaction.rolledBack)
			}
		})
	}

	// 审计写入失败必须回滚状态变更。
	auditFailure := errors.New("audit failed")
	transaction := &runtimeStateTestTx{
		rows:    []pgx.Row{runtimeStateTestRow{values: []any{false, int64(1)}}, runtimeStateTestRow{values: []any{"echo", true, int64(2), time.Now().UTC()}}},
		execErr: auditFailure,
	}
	repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
	if _, err := repository.UpdateDesiredEnabled(context.Background(), runtimeStateTestActor, "echo", true, 1); !errors.Is(err, auditFailure) {
		t.Fatalf("audit failure error = %v", err)
	}
	if transaction.committed || !transaction.rolledBack {
		t.Fatal("audit failure did not roll back")
	}

	commitFailure := errors.New("commit failed")
	transaction = &runtimeStateTestTx{
		rows:      []pgx.Row{runtimeStateTestRow{values: []any{false, int64(1)}}, runtimeStateTestRow{values: []any{"echo", true, int64(2), time.Now().UTC()}}},
		commitErr: commitFailure,
	}
	repository = &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
	if _, err := repository.UpdateDesiredEnabled(context.Background(), runtimeStateTestActor, "echo", true, 1); !errors.Is(err, commitFailure) {
		t.Fatalf("commit failure error = %v", err)
	}

	beginFailure := errors.New("begin failed")
	repository = &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{beginErr: beginFailure}}
	if _, err := repository.UpdateDesiredEnabled(context.Background(), runtimeStateTestActor, "echo", true, 1); !errors.Is(err, beginFailure) {
		t.Fatalf("begin failure error = %v", err)
	}
	if _, err := repository.SetGroupEnabled(context.Background(), runtimeStateTestActor, "echo", 100, true, 0); !errors.Is(err, beginFailure) {
		t.Fatalf("group begin failure error = %v", err)
	}

	// 外键失败表示插件全局状态行缺失，而不是版本冲突。
	foreignKeyError := &pgconn.PgError{Code: "23503", ConstraintName: "plugin_group_states_plugin_key_fkey"}
	transaction = &runtimeStateTestTx{rows: []pgx.Row{runtimeStateTestRow{err: foreignKeyError}}}
	repository = &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
	if _, err := repository.SetGroupEnabled(context.Background(), runtimeStateTestActor, "missing", 100, true, 0); !errors.Is(err, ErrRuntimeStateNotFound) {
		t.Fatalf("foreign key error = %v", err)
	}

	// 参数校验必须在开启事务之前拒绝。
	database := &runtimeStateTestDatabase{}
	repository = &PostgresRuntimeStateRepository{database: database}
	if _, err := repository.UpdateDesiredEnabled(context.Background(), runtimeStateTestActor, "bad-key", true, 1); err == nil {
		t.Fatal("invalid global plugin key accepted")
	}
	if _, err := repository.UpdateDesiredEnabled(context.Background(), runtimeStateTestActor, "echo", true, 0); !errors.Is(err, ErrRuntimeStateInvalidVersion) {
		t.Fatalf("invalid global version error = %v", err)
	}
	if _, err := repository.SetGroupEnabled(context.Background(), runtimeStateTestActor, "bad-key", 100, true, 0); err == nil {
		t.Fatal("invalid group plugin key accepted")
	}
	if _, err := repository.SetGroupEnabled(context.Background(), runtimeStateTestActor, "echo", 0, true, 0); !errors.Is(err, ErrInvalidRuntimeGroupID) {
		t.Fatalf("invalid group error = %v", err)
	}
	if _, err := repository.SetGroupEnabled(context.Background(), runtimeStateTestActor, "echo", 100, true, -1); !errors.Is(err, ErrRuntimeStateInvalidVersion) {
		t.Fatalf("invalid group version error = %v", err)
	}
	if database.tx != nil {
		t.Fatal("validation failure started a transaction")
	}
}

func TestRuntimeStateRepositoryCopiesCompleteSourceSetWithOrderedSerializableLock(t *testing.T) {
	zone := time.FixedZone("copy", 8*60*60)
	updatedAt := time.Date(2026, 7, 28, 12, 0, 0, 0, zone)
	locked := &runtimeStateTestRows{values: [][]any{
		{"echo", int64(100), false, int64(1), updatedAt},
		{"echo", int64(200), true, int64(2), updatedAt},
		{"tools", int64(200), false, int64(3), updatedAt},
	}}
	transaction := &runtimeStateTestTx{
		queryRows: locked,
		rows: []pgx.Row{
			runtimeStateTestRow{values: []any{true, int64(2), updatedAt}},
			runtimeStateTestRow{values: []any{false, int64(1), updatedAt}},
		},
	}
	database := &runtimeStateTestDatabase{tx: transaction}
	repository := &PostgresRuntimeStateRepository{database: database}
	states, err := repository.CopyGroupStates(context.Background(), runtimeStateTestActor, 200, 100,
		[]string{"tools", "echo"},
		[]RuntimeGroupCopyItem{
			{PluginKey: "tools", Enabled: false, SourceVersion: 3, TargetVersion: 0},
			{PluginKey: "echo", Enabled: true, SourceVersion: 2, TargetVersion: 1},
		})
	if err != nil {
		t.Fatal(err)
	}
	if database.txOptions.IsoLevel != pgx.Serializable {
		t.Fatalf("transaction isolation = %v", database.txOptions.IsoLevel)
	}
	if !strings.Contains(normalizeRuntimeStateSQL(transaction.querySQL), "ORDER BY plugin_key,group_id FOR UPDATE") {
		t.Fatalf("lock query = %q", transaction.querySQL)
	}
	if !reflect.DeepEqual(transaction.queryArgs, []any{[]string{"echo", "tools"}, []int64{100, 200}}) {
		t.Fatalf("lock args = %#v", transaction.queryArgs)
	}
	if !locked.closed {
		t.Fatal("locked rows not closed")
	}
	if len(transaction.rowSQL) != 2 || !strings.Contains(transaction.rowSQL[0], "UPDATE plugin_group_states") || !strings.Contains(transaction.rowSQL[1], "INSERT INTO plugin_group_states") {
		t.Fatalf("write SQL = %#v", transaction.rowSQL)
	}
	if len(states) != 2 || states[0].PluginKey != "echo" || states[1].PluginKey != "tools" || states[0].UpdatedAt.Location() != time.UTC {
		t.Fatalf("states = %+v", states)
	}
	if transaction.execCalls != 1 || !transaction.committed || transaction.rolledBack {
		t.Fatalf("audit calls=%d committed=%t rolledBack=%t", transaction.execCalls, transaction.committed, transaction.rolledBack)
	}
	before, _ := transaction.execArgs[6].([]byte)
	after, _ := transaction.execArgs[7].([]byte)
	if !strings.Contains(string(before), `"source_states"`) || !strings.Contains(string(before), `"target_states"`) || !strings.Contains(string(before), `"plugin_key":"tools","group_id":100`) || !strings.Contains(string(before), `"exists":false`) {
		t.Fatalf("copy audit before = %s", before)
	}
	if !strings.Contains(string(after), `"plugin_key":"tools","group_id":100`) || strings.Contains(string(after), `"exists":false`) {
		t.Fatalf("copy audit after = %s", after)
	}
}

func TestRuntimeStateRepositoryCopyNoChangeSkipsAudit(t *testing.T) {
	updatedAt := time.Now().UTC()
	transaction := &runtimeStateTestTx{queryRows: &runtimeStateTestRows{values: [][]any{
		{"echo", int64(100), true, int64(4), updatedAt},
		{"echo", int64(200), true, int64(2), updatedAt},
	}}}
	repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
	states, err := repository.CopyGroupStates(context.Background(), runtimeStateTestActor, 200, 100,
		[]string{"echo"}, []RuntimeGroupCopyItem{{PluginKey: "echo", Enabled: true, SourceVersion: 2, TargetVersion: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Version != 4 || len(transaction.rowSQL) != 0 || transaction.execCalls != 0 || !transaction.committed {
		t.Fatalf("no-op states=%+v writes=%d audits=%d committed=%t", states, len(transaction.rowSQL), transaction.execCalls, transaction.committed)
	}
}

func TestRuntimeStateRepositoryCopyRejectsIncompleteOrStaleSourceSet(t *testing.T) {
	updatedAt := time.Now().UTC()
	tests := []struct {
		name string
		rows [][]any
		item RuntimeGroupCopyItem
	}{
		{
			name: "omitted source",
			rows: [][]any{{"echo", int64(200), true, int64(2), updatedAt}, {"tools", int64(200), false, int64(3), updatedAt}},
			item: RuntimeGroupCopyItem{PluginKey: "echo", Enabled: true, SourceVersion: 2},
		},
		{
			name: "changed source",
			rows: [][]any{{"echo", int64(200), false, int64(3), updatedAt}},
			item: RuntimeGroupCopyItem{PluginKey: "echo", Enabled: true, SourceVersion: 2},
		},
		{
			name: "stale target",
			rows: [][]any{{"echo", int64(100), false, int64(4), updatedAt}, {"echo", int64(200), true, int64(2), updatedAt}},
			item: RuntimeGroupCopyItem{PluginKey: "echo", Enabled: true, SourceVersion: 2, TargetVersion: 3},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transaction := &runtimeStateTestTx{queryRows: &runtimeStateTestRows{values: test.rows}}
			repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
			if _, err := repository.CopyGroupStates(context.Background(), runtimeStateTestActor, 200, 100, []string{"echo", "tools"}, []RuntimeGroupCopyItem{test.item}); !errors.Is(err, ErrRuntimeStateConflict) {
				t.Fatalf("CopyGroupStates() error = %v", err)
			}
			if transaction.committed || !transaction.rolledBack || transaction.execCalls != 0 || len(transaction.rowSQL) != 0 {
				t.Fatalf("conflict transaction = %+v", transaction)
			}
		})
	}
}

func TestRuntimeStateRepositoryCopyRollsBackAuditAndMapsConflicts(t *testing.T) {
	updatedAt := time.Now().UTC()
	newTransaction := func() *runtimeStateTestTx {
		return &runtimeStateTestTx{
			queryRows: &runtimeStateTestRows{values: [][]any{{"echo", int64(200), true, int64(2), updatedAt}}},
			rows:      []pgx.Row{runtimeStateTestRow{values: []any{true, int64(1), updatedAt}}},
		}
	}
	auditFailure := errors.New("audit failed")
	transaction := newTransaction()
	transaction.execErr = auditFailure
	repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
	if _, err := repository.CopyGroupStates(context.Background(), runtimeStateTestActor, 200, 100, []string{"echo"}, []RuntimeGroupCopyItem{{PluginKey: "echo", Enabled: true, SourceVersion: 2}}); !errors.Is(err, auditFailure) {
		t.Fatalf("audit error = %v", err)
	}
	if transaction.committed || !transaction.rolledBack {
		t.Fatal("audit failure did not roll back")
	}
	transaction = newTransaction()
	transaction.execErr = &pgconn.PgError{Code: "40P01"}
	repository = &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
	if _, err := repository.CopyGroupStates(context.Background(), runtimeStateTestActor, 200, 100, []string{"echo"}, []RuntimeGroupCopyItem{{PluginKey: "echo", Enabled: true, SourceVersion: 2}}); !errors.Is(err, ErrRuntimeStateConflict) {
		t.Fatalf("audit conflict = %v", err)
	}

	transaction = newTransaction()
	transaction.commitErr = &pgconn.PgError{Code: "40001"}
	repository = &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
	if _, err := repository.CopyGroupStates(context.Background(), runtimeStateTestActor, 200, 100, []string{"echo"}, []RuntimeGroupCopyItem{{PluginKey: "echo", Enabled: true, SourceVersion: 2}}); !errors.Is(err, ErrRuntimeStateConflict) {
		t.Fatalf("commit conflict = %v", err)
	}

	commitFailure := errors.New("connection lost")
	transaction = newTransaction()
	transaction.commitErr = commitFailure
	repository = &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
	if _, err := repository.CopyGroupStates(context.Background(), runtimeStateTestActor, 200, 100, []string{"echo"}, []RuntimeGroupCopyItem{{PluginKey: "echo", Enabled: true, SourceVersion: 2}}); !errors.Is(err, ErrRuntimeStateCommitUnknown) || !errors.Is(err, commitFailure) {
		t.Fatalf("unknown commit error = %v", err)
	}

	transaction = &runtimeStateTestTx{queryErr: &pgconn.PgError{Code: "40P01"}}
	repository = &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
	if _, err := repository.CopyGroupStates(context.Background(), runtimeStateTestActor, 200, 100, []string{"echo"}, []RuntimeGroupCopyItem{{PluginKey: "echo", Enabled: true, SourceVersion: 2}}); !errors.Is(err, ErrRuntimeStateConflict) {
		t.Fatalf("lock conflict = %v", err)
	}
}

func TestRuntimeStateRepositoryCleanupUsesAuthorityAndSingleAudit(t *testing.T) {
	zone := time.FixedZone("cleanup", -5*60*60)
	updatedAt := time.Date(2026, 7, 28, 3, 0, 0, 0, zone)
	transaction := &runtimeStateTestTx{rows: []pgx.Row{
		runtimeStateTestRow{values: []any{true, int64(2), updatedAt}},
		runtimeStateTestRow{values: []any{false, int64(3), updatedAt}},
		runtimeStateTestRow{values: []any{int64(2)}},
		runtimeStateTestRow{values: []any{int64(3)}},
	}}
	database := &runtimeStateTestDatabase{tx: transaction}
	repository := &PostgresRuntimeStateRepository{database: database}
	deleted, err := repository.CleanupGroupStates(context.Background(), runtimeStateTestActor, []RuntimeGroupStateRecord{
		{PluginKey: "tools", GroupID: 300, Enabled: true, Version: 3},
		{PluginKey: "echo", GroupID: 200, Enabled: false, Version: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if database.txOptions.IsoLevel != pgx.Serializable || transaction.execCalls != 1 || !transaction.committed {
		t.Fatalf("isolation=%v audit=%d committed=%t", database.txOptions.IsoLevel, transaction.execCalls, transaction.committed)
	}
	if len(transaction.rowArgs) != 4 || !reflect.DeepEqual(transaction.rowArgs[0], []any{"echo", int64(200)}) || !reflect.DeepEqual(transaction.rowArgs[1], []any{"tools", int64(300)}) {
		t.Fatalf("lock order args = %#v", transaction.rowArgs)
	}
	if len(deleted) != 2 || deleted[0].PluginKey != "echo" || !deleted[0].Enabled || deleted[1].PluginKey != "tools" || deleted[1].Enabled || deleted[0].UpdatedAt.Location() != time.UTC {
		t.Fatalf("authoritative deleted = %+v", deleted)
	}
	if !reflect.DeepEqual(transaction.execArgs[3:6], []any{"plugin.runtime.group.cleanup", "plugin_runtime_group", "batch"}) {
		t.Fatalf("audit args = %#v", transaction.execArgs)
	}
	before, _ := transaction.execArgs[6].([]byte)
	if !strings.Contains(string(before), `"enabled":true`) || !strings.Contains(string(before), `"enabled":false`) {
		t.Fatalf("cleanup audit before = %s", before)
	}
}

func TestRuntimeStateRepositoryCleanupRollsBackConflictsAndUnknownCommit(t *testing.T) {
	updatedAt := time.Now().UTC()
	expected := []RuntimeGroupStateRecord{
		{PluginKey: "echo", GroupID: 200, Version: 2},
		{PluginKey: "tools", GroupID: 300, Version: 3},
	}

	t.Run("second CAS conflict", func(t *testing.T) {
		transaction := &runtimeStateTestTx{rows: []pgx.Row{
			runtimeStateTestRow{values: []any{true, int64(2), updatedAt}},
			runtimeStateTestRow{err: pgx.ErrNoRows},
		}}
		repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
		if _, err := repository.CleanupGroupStates(context.Background(), runtimeStateTestActor, expected); !errors.Is(err, ErrRuntimeStateConflict) {
			t.Fatalf("CleanupGroupStates() error = %v", err)
		}
		if transaction.committed || !transaction.rolledBack || transaction.execCalls != 0 || len(transaction.rowSQL) != 2 {
			t.Fatalf("conflict transaction = %+v", transaction)
		}
	})

	t.Run("delete CAS conflict", func(t *testing.T) {
		transaction := &runtimeStateTestTx{rows: []pgx.Row{
			runtimeStateTestRow{values: []any{true, int64(2), updatedAt}},
			runtimeStateTestRow{values: []any{false, int64(3), updatedAt}},
			runtimeStateTestRow{values: []any{int64(2)}},
			runtimeStateTestRow{err: pgx.ErrNoRows},
		}}
		repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
		if _, err := repository.CleanupGroupStates(context.Background(), runtimeStateTestActor, expected); !errors.Is(err, ErrRuntimeStateConflict) {
			t.Fatalf("CleanupGroupStates() error = %v", err)
		}
		if transaction.committed || !transaction.rolledBack || transaction.execCalls != 0 || len(transaction.rowSQL) != 4 {
			t.Fatalf("delete conflict transaction = %+v", transaction)
		}
	})

	newSingleTransaction := func() *runtimeStateTestTx {
		return &runtimeStateTestTx{rows: []pgx.Row{
			runtimeStateTestRow{values: []any{true, int64(2), updatedAt}},
			runtimeStateTestRow{values: []any{int64(2)}},
		}}
	}
	one := expected[:1]
	t.Run("audit failure", func(t *testing.T) {
		auditFailure := errors.New("audit failed")
		transaction := newSingleTransaction()
		transaction.execErr = auditFailure
		repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
		if _, err := repository.CleanupGroupStates(context.Background(), runtimeStateTestActor, one); !errors.Is(err, auditFailure) {
			t.Fatalf("CleanupGroupStates() error = %v", err)
		}
		if transaction.committed || !transaction.rolledBack || transaction.execCalls != 1 {
			t.Fatalf("audit failure transaction = %+v", transaction)
		}
	})

	t.Run("commit unknown", func(t *testing.T) {
		commitFailure := errors.New("connection lost")
		transaction := newSingleTransaction()
		transaction.commitErr = commitFailure
		repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
		if _, err := repository.CleanupGroupStates(context.Background(), runtimeStateTestActor, one); !errors.Is(err, ErrRuntimeStateCommitUnknown) || !errors.Is(err, commitFailure) {
			t.Fatalf("CleanupGroupStates() error = %v", err)
		}
	})

	t.Run("lock deadlock", func(t *testing.T) {
		transaction := &runtimeStateTestTx{rows: []pgx.Row{runtimeStateTestRow{err: &pgconn.PgError{Code: "40P01"}}}}
		repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{tx: transaction}}
		if _, err := repository.CleanupGroupStates(context.Background(), runtimeStateTestActor, one); !errors.Is(err, ErrRuntimeStateConflict) {
			t.Fatalf("CleanupGroupStates() error = %v", err)
		}
	})
}

func TestRuntimeStateRepositoryBatchValidationAndConflictMapping(t *testing.T) {
	repository := &PostgresRuntimeStateRepository{database: &runtimeStateTestDatabase{}}
	if _, err := repository.CopyGroupStates(context.Background(), runtimeStateTestActor, 100, 200, nil, []RuntimeGroupCopyItem{{PluginKey: "echo", SourceVersion: 1}}); !errors.Is(err, ErrRuntimePluginNotFound) {
		t.Fatalf("missing allowed keys error = %v", err)
	}
	if _, err := repository.CopyGroupStates(context.Background(), runtimeStateTestActor, 100, 200, []string{"echo", "echo"}, []RuntimeGroupCopyItem{{PluginKey: "echo", SourceVersion: 1}}); !errors.Is(err, ErrRuntimeGroupStateDuplicate) {
		t.Fatalf("duplicate allowed keys error = %v", err)
	}
	if _, err := repository.CleanupGroupStates(context.Background(), runtimeStateTestActor, []RuntimeGroupStateRecord{{PluginKey: "echo", GroupID: 100, Version: 0}}); !errors.Is(err, ErrRuntimeStateInvalidVersion) {
		t.Fatalf("cleanup version error = %v", err)
	}
	for _, code := range []string{"23505", "40001", "40P01"} {
		if err := mapRuntimeBatchWriteError("test", &pgconn.PgError{Code: code}); !errors.Is(err, ErrRuntimeStateConflict) {
			t.Fatalf("code %s mapped to %v", code, err)
		}
	}
	other := errors.New("other")
	if err := mapRuntimeBatchWriteError("test", other); !errors.Is(err, other) || errors.Is(err, ErrRuntimeStateConflict) {
		t.Fatalf("other mapped to %v", err)
	}
	if err := mapRuntimeBatchCommitError("test", other); !errors.Is(err, ErrRuntimeStateCommitUnknown) || !errors.Is(err, other) {
		t.Fatalf("unknown commit mapped to %v", err)
	}
}

func normalizeRuntimeStateSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}
