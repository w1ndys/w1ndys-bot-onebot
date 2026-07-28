package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/w1ndys-bot/internal/management"
)

type runtimeStateDatabase interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

var (
	ErrRuntimeStateConflict       = errors.New("插件状态版本冲突")
	ErrRuntimeStateInvalidVersion = errors.New("插件状态版本无效")
	ErrRuntimeStateCommitUnknown  = errors.New("插件状态事务提交结果未知")
	ErrRuntimeStateNotFound       = errors.New("插件状态不存在")
	ErrRuntimeConfigNotFound      = errors.New("插件配置不存在")
)

const RuntimeGroupBatchLimit = 1000

// RuntimeGroupStateKey 唯一标识一条插件逐群开关记录。
type RuntimeGroupStateKey struct {
	PluginKey string `json:"plugin_key"`
	GroupID   int64  `json:"group_id"`
}

// RuntimeGroupCopyItem 是复制预览产生并由执行请求原样回传的 CAS 项。
type RuntimeGroupCopyItem struct {
	PluginKey     string `json:"plugin_key"`
	Enabled       bool   `json:"enabled"`
	SourceVersion int64  `json:"source_version"`
	TargetVersion int64  `json:"target_version"`
	TargetEnabled *bool  `json:"target_enabled,omitempty"`
}

// RuntimeGroupStateRecord 是带插件 Key 的权威逐群状态。
type RuntimeGroupStateRecord struct {
	PluginKey string    `json:"plugin_key"`
	GroupID   int64     `json:"group_id"`
	Enabled   bool      `json:"enabled"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

type runtimeGroupBatchAudit struct {
	SourceGroupID int64                     `json:"source_group_id,omitempty"`
	TargetGroupID int64                     `json:"target_group_id,omitempty"`
	States        []RuntimeGroupStateRecord `json:"states"`
}

type runtimeGroupAuditState struct {
	PluginKey string     `json:"plugin_key"`
	GroupID   int64      `json:"group_id"`
	Enabled   bool       `json:"enabled"`
	Version   int64      `json:"version"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	Exists    bool       `json:"exists"`
}

type runtimeGroupCopyAudit struct {
	SourceGroupID int64                    `json:"source_group_id"`
	TargetGroupID int64                    `json:"target_group_id"`
	SourceStates  []runtimeGroupAuditState `json:"source_states"`
	TargetStates  []runtimeGroupAuditState `json:"target_states"`
}

// PersistedPluginConfig 是插件已持久化的原始配置值与乐观锁版本。
type PersistedPluginConfig struct {
	PluginKey  string
	ConfigJSON json.RawMessage
	Version    int64
	UpdatedAt  time.Time
}

// runtimeConfigAudit 是配置变更写入审计的前后快照。
type runtimeConfigAudit struct {
	PluginKey  string          `json:"plugin_key"`
	ConfigJSON json.RawMessage `json:"config_json"`
	Version    int64           `json:"version"`
}

// runtimeStateAudit 是全局意图变更写入审计的有界前后快照。
type runtimeStateAudit struct {
	PluginKey      string `json:"plugin_key"`
	DesiredEnabled bool   `json:"desired_enabled"`
	Version        int64  `json:"version"`
}

// runtimeGroupAudit 是逐群开关变更写入审计的有界前后快照。
type runtimeGroupAudit struct {
	PluginKey string `json:"plugin_key"`
	GroupID   int64  `json:"group_id"`
	Enabled   bool   `json:"enabled"`
	Version   int64  `json:"version"`
}

// PersistedGroupState 是一个插件的显式逐群开关快照。
type PersistedGroupState struct {
	GroupID   int64
	Enabled   bool
	Version   int64
	UpdatedAt time.Time
}

// PersistedPluginState 是管理员意图及其全部逐群开关快照。
type PersistedPluginState struct {
	PluginKey      string
	DesiredEnabled bool
	Version        int64
	UpdatedAt      time.Time
	Groups         []PersistedGroupState
}

// PostgresRuntimeStateRepository 持久化 V2 插件运行意图。
type PostgresRuntimeStateRepository struct {
	database runtimeStateDatabase
}

// NewPostgresRuntimeStateRepository 创建 V2 插件状态仓库。
func NewPostgresRuntimeStateRepository(pool *pgxpool.Pool) (*PostgresRuntimeStateRepository, error) {
	if pool == nil {
		return nil, fmt.Errorf("插件状态数据库连接池不能为空")
	}
	return &PostgresRuntimeStateRepository{database: pool}, nil
}

// SyncCatalog 为当前编译目录补充默认关闭的全局状态，不覆盖已有意图。
func (r *PostgresRuntimeStateRepository) SyncCatalog(ctx context.Context, catalog *SpecCatalog) error {
	if r == nil || r.database == nil {
		return fmt.Errorf("插件状态仓库未初始化")
	}
	if catalog == nil {
		return fmt.Errorf("插件规格目录不能为空")
	}
	specs := catalog.Specs()
	if len(specs) == 0 {
		return nil
	}
	keys := make([]string, len(specs))
	for index, spec := range specs {
		keys[index] = spec.Key
	}
	_, err := r.database.Exec(ctx, `INSERT INTO plugin_states(plugin_key,desired_enabled)
SELECT plugin_key,FALSE FROM unnest($1::text[]) AS plugin_key
ON CONFLICT (plugin_key) DO NOTHING`, keys)
	if err != nil {
		return fmt.Errorf("同步插件状态目录: %w", err)
	}
	configured := make([]string, 0, len(specs))
	for _, spec := range specs {
		if spec.Config != nil {
			configured = append(configured, spec.Key)
		}
	}
	if len(configured) == 0 {
		return nil
	}
	// 配置行随目录预建，使后续写入始终走正版本 CAS，无需区分新增与更新。
	_, err = r.database.Exec(ctx, `INSERT INTO plugin_runtime_configs(plugin_key)
SELECT plugin_key FROM unnest($1::text[]) AS plugin_key
ON CONFLICT (plugin_key) DO NOTHING`, configured)
	if err != nil {
		return fmt.Errorf("同步插件配置目录: %w", err)
	}
	return nil
}

// FindConfig 读取插件已持久化的原始配置值与乐观锁版本。
func (r *PostgresRuntimeStateRepository) FindConfig(ctx context.Context, pluginKey string) (PersistedPluginConfig, error) {
	if r == nil || r.database == nil {
		return PersistedPluginConfig{}, fmt.Errorf("插件状态仓库未初始化")
	}
	if !identifierPattern.MatchString(pluginKey) {
		return PersistedPluginConfig{}, fmt.Errorf("无效插件 Key %q", pluginKey)
	}
	var config PersistedPluginConfig
	err := r.database.QueryRow(ctx, `SELECT plugin_key,config_json,version,updated_at FROM plugin_runtime_configs WHERE plugin_key=$1`, pluginKey).
		Scan(&config.PluginKey, &config.ConfigJSON, &config.Version, &config.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PersistedPluginConfig{}, fmt.Errorf("%w: %s", ErrRuntimeConfigNotFound, pluginKey)
	}
	if err != nil {
		return PersistedPluginConfig{}, fmt.Errorf("查询插件 %s 配置: %w", pluginKey, err)
	}
	config.UpdatedAt = config.UpdatedAt.UTC()
	return config, nil
}

// LoadConfigs 一次读取全部插件配置，供启动期热应用使用。
func (r *PostgresRuntimeStateRepository) LoadConfigs(ctx context.Context) ([]PersistedPluginConfig, error) {
	if r == nil || r.database == nil {
		return nil, fmt.Errorf("插件状态仓库未初始化")
	}
	rows, err := r.database.Query(ctx, `SELECT plugin_key,config_json,version,updated_at FROM plugin_runtime_configs ORDER BY plugin_key`)
	if err != nil {
		return nil, fmt.Errorf("查询插件配置快照: %w", err)
	}
	defer rows.Close()

	configs := make([]PersistedPluginConfig, 0)
	for rows.Next() {
		var config PersistedPluginConfig
		if err := rows.Scan(&config.PluginKey, &config.ConfigJSON, &config.Version, &config.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描插件配置快照: %w", err)
		}
		config.UpdatedAt = config.UpdatedAt.UTC()
		configs = append(configs, config)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历插件配置快照: %w", err)
	}
	return configs, nil
}

// SaveConfig 使用乐观锁写入插件配置，并在同一事务写入审计。
func (r *PostgresRuntimeStateRepository) SaveConfig(ctx context.Context, actor management.Actor, pluginKey string, schema ConfigSchema, configJSON json.RawMessage, expectedVersion int64) (PersistedPluginConfig, error) {
	if r == nil || r.database == nil {
		return PersistedPluginConfig{}, fmt.Errorf("插件状态仓库未初始化")
	}
	if !identifierPattern.MatchString(pluginKey) {
		return PersistedPluginConfig{}, fmt.Errorf("无效插件 Key %q", pluginKey)
	}
	if expectedVersion <= 0 {
		return PersistedPluginConfig{}, ErrRuntimeStateInvalidVersion
	}
	tx, err := r.database.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return PersistedPluginConfig{}, fmt.Errorf("开启插件配置事务: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	before := runtimeConfigAudit{PluginKey: pluginKey}
	err = tx.QueryRow(ctx, `SELECT config_json,version FROM plugin_runtime_configs WHERE plugin_key=$1 FOR UPDATE`, pluginKey).
		Scan(&before.ConfigJSON, &before.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return PersistedPluginConfig{}, fmt.Errorf("%w: %s", ErrRuntimeConfigNotFound, pluginKey)
	}
	if err != nil {
		return PersistedPluginConfig{}, fmt.Errorf("锁定插件 %s 配置: %w", pluginKey, err)
	}
	if before.Version != expectedVersion {
		return PersistedPluginConfig{}, ErrRuntimeStateConflict
	}
	var saved PersistedPluginConfig
	err = tx.QueryRow(ctx, `UPDATE plugin_runtime_configs
SET config_json=$2,version=version+1,updated_at=NOW()
WHERE plugin_key=$1 AND version=$3
RETURNING plugin_key,config_json,version,updated_at`, pluginKey, []byte(configJSON), expectedVersion).
		Scan(&saved.PluginKey, &saved.ConfigJSON, &saved.Version, &saved.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PersistedPluginConfig{}, ErrRuntimeStateConflict
	}
	if err != nil {
		return PersistedPluginConfig{}, fmt.Errorf("更新插件配置: %w", err)
	}
	redactedBefore, err := RedactConfig(schema, before.ConfigJSON)
	if err != nil {
		return PersistedPluginConfig{}, fmt.Errorf("脱敏插件 %s 原配置审计快照: %w", pluginKey, err)
	}
	redactedAfter, err := RedactConfig(schema, saved.ConfigJSON)
	if err != nil {
		return PersistedPluginConfig{}, fmt.Errorf("脱敏插件 %s 新配置审计快照: %w", pluginKey, err)
	}
	before.ConfigJSON = redactedBefore
	after := runtimeConfigAudit{PluginKey: pluginKey, ConfigJSON: redactedAfter, Version: saved.Version}
	if err := recordRuntimeAudit(ctx, tx, actor, "plugin.runtime.config.update", "plugin_runtime_config", pluginKey, before, after); err != nil {
		return PersistedPluginConfig{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PersistedPluginConfig{}, fmt.Errorf("提交插件配置事务: %w", err)
	}
	saved.UpdatedAt = saved.UpdatedAt.UTC()
	return saved, nil
}

const runtimeStateSnapshotQuery = `SELECT s.plugin_key,s.desired_enabled,s.version,s.updated_at,
       g.group_id,g.enabled,g.version,g.updated_at
FROM plugin_states s
LEFT JOIN plugin_group_states g ON g.plugin_key=s.plugin_key`

// LoadSnapshot 一次查询读取全部全局意图和逐群开关。
func (r *PostgresRuntimeStateRepository) LoadSnapshot(ctx context.Context) ([]PersistedPluginState, error) {
	if r == nil || r.database == nil {
		return nil, fmt.Errorf("插件状态仓库未初始化")
	}
	rows, err := r.database.Query(ctx, runtimeStateSnapshotQuery+` ORDER BY s.plugin_key,g.group_id`)
	if err != nil {
		return nil, fmt.Errorf("查询插件状态快照: %w", err)
	}
	return scanRuntimeStates(rows)
}

// FindState 读取单个插件的全局意图及其全部逐群开关。
func (r *PostgresRuntimeStateRepository) FindState(ctx context.Context, pluginKey string) (PersistedPluginState, error) {
	if r == nil || r.database == nil {
		return PersistedPluginState{}, fmt.Errorf("插件状态仓库未初始化")
	}
	if !identifierPattern.MatchString(pluginKey) {
		return PersistedPluginState{}, fmt.Errorf("无效插件 Key %q", pluginKey)
	}
	rows, err := r.database.Query(ctx, runtimeStateSnapshotQuery+` WHERE s.plugin_key=$1 ORDER BY g.group_id`, pluginKey)
	if err != nil {
		return PersistedPluginState{}, fmt.Errorf("查询插件 %s 状态: %w", pluginKey, err)
	}
	states, err := scanRuntimeStates(rows)
	if err != nil {
		return PersistedPluginState{}, err
	}
	if len(states) == 0 {
		return PersistedPluginState{}, fmt.Errorf("%w: %s", ErrRuntimeStateNotFound, pluginKey)
	}
	return states[0], nil
}

func scanRuntimeStates(rows pgx.Rows) ([]PersistedPluginState, error) {
	defer rows.Close()

	states := make([]PersistedPluginState, 0)
	for rows.Next() {
		var pluginKey string
		var desiredEnabled bool
		var pluginVersion int64
		var pluginUpdatedAt time.Time
		var groupID *int64
		var groupEnabled *bool
		var groupVersion *int64
		var groupUpdatedAt *time.Time
		if err := rows.Scan(&pluginKey, &desiredEnabled, &pluginVersion, &pluginUpdatedAt, &groupID, &groupEnabled, &groupVersion, &groupUpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描插件状态快照: %w", err)
		}
		if len(states) == 0 || states[len(states)-1].PluginKey != pluginKey {
			states = append(states, PersistedPluginState{
				PluginKey: pluginKey, DesiredEnabled: desiredEnabled, Version: pluginVersion,
				UpdatedAt: pluginUpdatedAt.UTC(), Groups: make([]PersistedGroupState, 0),
			})
		}
		if groupID != nil && groupEnabled != nil && groupVersion != nil && groupUpdatedAt != nil {
			last := &states[len(states)-1]
			last.Groups = append(last.Groups, PersistedGroupState{
				GroupID: *groupID, Enabled: *groupEnabled, Version: *groupVersion, UpdatedAt: groupUpdatedAt.UTC(),
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历插件状态快照: %w", err)
	}
	return states, nil
}

// UpdateDesiredEnabled 使用乐观锁更新插件全局启用意图，并在同一事务写入审计。
func (r *PostgresRuntimeStateRepository) UpdateDesiredEnabled(ctx context.Context, actor management.Actor, pluginKey string, enabled bool, expectedVersion int64) (PersistedPluginState, error) {
	if r == nil || r.database == nil {
		return PersistedPluginState{}, fmt.Errorf("插件状态仓库未初始化")
	}
	if !identifierPattern.MatchString(pluginKey) {
		return PersistedPluginState{}, fmt.Errorf("无效插件 Key %q", pluginKey)
	}
	if expectedVersion <= 0 {
		return PersistedPluginState{}, ErrRuntimeStateInvalidVersion
	}
	tx, err := r.database.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return PersistedPluginState{}, fmt.Errorf("开启插件全局状态事务: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// 先锁行取旧值，使审计 before 与 CAS 基线来自同一可信快照。
	before := runtimeStateAudit{PluginKey: pluginKey}
	err = tx.QueryRow(ctx, `SELECT desired_enabled,version FROM plugin_states WHERE plugin_key=$1 FOR UPDATE`, pluginKey).
		Scan(&before.DesiredEnabled, &before.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return PersistedPluginState{}, fmt.Errorf("%w: %s", ErrRuntimeStateNotFound, pluginKey)
	}
	if err != nil {
		return PersistedPluginState{}, fmt.Errorf("锁定插件 %s 全局状态: %w", pluginKey, err)
	}
	if before.Version != expectedVersion {
		return PersistedPluginState{}, ErrRuntimeStateConflict
	}
	var state PersistedPluginState
	err = tx.QueryRow(ctx, `UPDATE plugin_states
SET desired_enabled=$2,version=version+1,updated_at=NOW()
WHERE plugin_key=$1 AND version=$3
RETURNING plugin_key,desired_enabled,version,updated_at`, pluginKey, enabled, expectedVersion).
		Scan(&state.PluginKey, &state.DesiredEnabled, &state.Version, &state.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PersistedPluginState{}, ErrRuntimeStateConflict
	}
	if err != nil {
		return PersistedPluginState{}, fmt.Errorf("更新插件全局状态: %w", err)
	}
	after := runtimeStateAudit{PluginKey: pluginKey, DesiredEnabled: state.DesiredEnabled, Version: state.Version}
	if err := recordRuntimeAudit(ctx, tx, actor, "plugin.runtime.global.update", "plugin_runtime_state", pluginKey, before, after); err != nil {
		return PersistedPluginState{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PersistedPluginState{}, fmt.Errorf("提交插件全局状态事务: %w", err)
	}
	state.UpdatedAt = state.UpdatedAt.UTC()
	state.Groups = make([]PersistedGroupState, 0)
	return state, nil
}

// SetGroupEnabled 新增或使用乐观锁更新插件逐群开关，并在同一事务写入审计。
// expectedVersion 为 0 表示仅允许新增；正数表示按版本更新。
func (r *PostgresRuntimeStateRepository) SetGroupEnabled(ctx context.Context, actor management.Actor, pluginKey string, groupID int64, enabled bool, expectedVersion int64) (PersistedGroupState, error) {
	if r == nil || r.database == nil {
		return PersistedGroupState{}, fmt.Errorf("插件状态仓库未初始化")
	}
	if !identifierPattern.MatchString(pluginKey) {
		return PersistedGroupState{}, fmt.Errorf("无效插件 Key %q", pluginKey)
	}
	if groupID <= 0 {
		return PersistedGroupState{}, ErrInvalidRuntimeGroupID
	}
	if expectedVersion < 0 {
		return PersistedGroupState{}, ErrRuntimeStateInvalidVersion
	}
	tx, err := r.database.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return PersistedGroupState{}, fmt.Errorf("开启插件群状态事务: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var before any
	if expectedVersion > 0 {
		lock := runtimeGroupAudit{PluginKey: pluginKey, GroupID: groupID}
		lockErr := tx.QueryRow(ctx, `SELECT enabled,version FROM plugin_group_states WHERE plugin_key=$1 AND group_id=$2 FOR UPDATE`, pluginKey, groupID).
			Scan(&lock.Enabled, &lock.Version)
		// 旧记录缺失时，携带正数版本的更新只能是陈旧写入。
		if errors.Is(lockErr, pgx.ErrNoRows) {
			return PersistedGroupState{}, ErrRuntimeStateConflict
		}
		if lockErr != nil {
			return PersistedGroupState{}, fmt.Errorf("锁定插件 %s 群 %d 状态: %w", pluginKey, groupID, lockErr)
		}
		if lock.Version != expectedVersion {
			return PersistedGroupState{}, ErrRuntimeStateConflict
		}
		before = lock
	}
	var state PersistedGroupState
	var row pgx.Row
	if expectedVersion == 0 {
		row = tx.QueryRow(ctx, `INSERT INTO plugin_group_states(plugin_key,group_id,enabled)
VALUES($1,$2,$3)
ON CONFLICT (plugin_key,group_id) DO NOTHING
RETURNING group_id,enabled,version,updated_at`, pluginKey, groupID, enabled)
	} else {
		row = tx.QueryRow(ctx, `UPDATE plugin_group_states
SET enabled=$3,version=version+1,updated_at=NOW()
WHERE plugin_key=$1 AND group_id=$2 AND version=$4
RETURNING group_id,enabled,version,updated_at`, pluginKey, groupID, enabled, expectedVersion)
	}
	err = row.Scan(&state.GroupID, &state.Enabled, &state.Version, &state.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PersistedGroupState{}, ErrRuntimeStateConflict
	}
	if err != nil {
		var postgresError *pgconn.PgError
		// 外键失败表示插件全局状态行缺失，目录同步之前不应写入群开关。
		if errors.As(err, &postgresError) && postgresError.Code == "23503" {
			return PersistedGroupState{}, fmt.Errorf("%w: %s", ErrRuntimeStateNotFound, pluginKey)
		}
		return PersistedGroupState{}, fmt.Errorf("保存插件群状态: %w", err)
	}
	after := runtimeGroupAudit{PluginKey: pluginKey, GroupID: state.GroupID, Enabled: state.Enabled, Version: state.Version}
	target := fmt.Sprintf("%s:%d", pluginKey, groupID)
	if err := recordRuntimeAudit(ctx, tx, actor, "plugin.runtime.group.update", "plugin_runtime_group_state", target, before, after); err != nil {
		return PersistedGroupState{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PersistedGroupState{}, fmt.Errorf("提交插件群状态事务: %w", err)
	}
	state.UpdatedAt = state.UpdatedAt.UTC()
	return state, nil
}

// CopyGroupStates 使用源、目标版本 CAS 在单一事务内合并复制显式群开关并写一条汇总审计。
func (r *PostgresRuntimeStateRepository) CopyGroupStates(ctx context.Context, actor management.Actor, sourceGroupID, targetGroupID int64, allowedPluginKeys []string, items []RuntimeGroupCopyItem) ([]RuntimeGroupStateRecord, error) {
	if r == nil || r.database == nil {
		return nil, fmt.Errorf("插件状态仓库未初始化")
	}
	if err := validateRuntimeGroupCopyItems(sourceGroupID, targetGroupID, items); err != nil {
		return nil, err
	}
	allowedPluginKeys, err := validateRuntimeAllowedPluginKeys(allowedPluginKeys, items)
	if err != nil {
		return nil, err
	}
	sorted := append([]RuntimeGroupCopyItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PluginKey < sorted[j].PluginKey })
	tx, err := r.database.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("开启群开关复制事务: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	sources := make(map[string]RuntimeGroupStateRecord, len(sorted))
	targets := make(map[string]RuntimeGroupStateRecord, len(sorted))
	groupIDs := []int64{sourceGroupID, targetGroupID}
	sort.Slice(groupIDs, func(i, j int) bool { return groupIDs[i] < groupIDs[j] })
	rows, err := tx.Query(ctx, `SELECT plugin_key,group_id,enabled,version,updated_at
FROM plugin_group_states
WHERE plugin_key=ANY($1::text[]) AND group_id=ANY($2::bigint[])
ORDER BY plugin_key,group_id
FOR UPDATE`, allowedPluginKeys, groupIDs)
	if err != nil {
		return nil, mapRuntimeBatchWriteError("锁定源群和目标群状态", err)
	}
	defer rows.Close()
	for rows.Next() {
		var current RuntimeGroupStateRecord
		if err := rows.Scan(&current.PluginKey, &current.GroupID, &current.Enabled, &current.Version, &current.UpdatedAt); err != nil {
			return nil, fmt.Errorf("读取锁定的群开关状态: %w", err)
		}
		current.UpdatedAt = current.UpdatedAt.UTC()
		if current.GroupID == sourceGroupID {
			sources[current.PluginKey] = current
		} else if current.GroupID == targetGroupID {
			targets[current.PluginKey] = current
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapRuntimeBatchWriteError("遍历锁定的群开关状态", err)
	}
	if len(sources) != len(sorted) {
		return nil, ErrRuntimeStateConflict
	}
	sourceAudit := make([]runtimeGroupAuditState, 0, len(sorted))
	targetBeforeAudit := make([]runtimeGroupAuditState, 0, len(sorted))
	for _, item := range sorted {
		source, found := sources[item.PluginKey]
		if !found || source.Enabled != item.Enabled || source.Version != item.SourceVersion {
			return nil, ErrRuntimeStateConflict
		}
		target, targetFound := targets[item.PluginKey]
		if !targetFound {
			target = RuntimeGroupStateRecord{PluginKey: item.PluginKey, GroupID: targetGroupID}
			targets[item.PluginKey] = target
		}
		if target.Version != item.TargetVersion {
			return nil, ErrRuntimeStateConflict
		}
		sourceAudit = append(sourceAudit, runtimeGroupStateAudit(source, true))
		targetBeforeAudit = append(targetBeforeAudit, runtimeGroupStateAudit(target, target.Version > 0))
	}

	after := make([]RuntimeGroupStateRecord, 0, len(sorted))
	changed := 0
	for _, item := range sorted {
		target := targets[item.PluginKey]
		if target.Version > 0 && target.Enabled == item.Enabled {
			after = append(after, target)
			continue
		}
		var saved RuntimeGroupStateRecord
		changed++
		saved.PluginKey, saved.GroupID = item.PluginKey, targetGroupID
		if target.Version == 0 {
			err = tx.QueryRow(ctx, `INSERT INTO plugin_group_states(plugin_key,group_id,enabled)
VALUES($1,$2,$3) ON CONFLICT (plugin_key,group_id) DO NOTHING
RETURNING enabled,version,updated_at`, item.PluginKey, targetGroupID, item.Enabled).
				Scan(&saved.Enabled, &saved.Version, &saved.UpdatedAt)
		} else {
			err = tx.QueryRow(ctx, `UPDATE plugin_group_states SET enabled=$3,version=version+1,updated_at=NOW()
WHERE plugin_key=$1 AND group_id=$2 AND version=$4
RETURNING enabled,version,updated_at`, item.PluginKey, targetGroupID, item.Enabled, item.TargetVersion).
				Scan(&saved.Enabled, &saved.Version, &saved.UpdatedAt)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRuntimeStateConflict
		}
		if err != nil {
			return nil, mapRuntimeBatchWriteError("复制插件群开关", err)
		}
		saved.UpdatedAt = saved.UpdatedAt.UTC()
		after = append(after, saved)
	}
	if changed == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, mapRuntimeBatchCommitError("提交群开关复制事务", err)
		}
		return after, nil
	}
	targetAfterAudit := make([]runtimeGroupAuditState, 0, len(after))
	for _, state := range after {
		targetAfterAudit = append(targetAfterAudit, runtimeGroupStateAudit(state, true))
	}
	beforeAudit := runtimeGroupCopyAudit{SourceGroupID: sourceGroupID, TargetGroupID: targetGroupID, SourceStates: sourceAudit, TargetStates: targetBeforeAudit}
	afterAudit := runtimeGroupCopyAudit{SourceGroupID: sourceGroupID, TargetGroupID: targetGroupID, SourceStates: sourceAudit, TargetStates: targetAfterAudit}
	if err := recordRuntimeAudit(ctx, tx, actor, "plugin.runtime.group.copy", "plugin_runtime_group", fmt.Sprintf("%d", targetGroupID), beforeAudit, afterAudit); err != nil {
		return nil, mapRuntimeBatchWriteError("写入群开关复制审计", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapRuntimeBatchCommitError("提交群开关复制事务", err)
	}
	return after, nil
}

func runtimeGroupStateAudit(state RuntimeGroupStateRecord, exists bool) runtimeGroupAuditState {
	audit := runtimeGroupAuditState{
		PluginKey: state.PluginKey,
		GroupID:   state.GroupID,
		Enabled:   state.Enabled,
		Version:   state.Version,
		Exists:    exists,
	}
	if exists {
		updatedAt := state.UpdatedAt.UTC()
		audit.UpdatedAt = &updatedAt
	}
	return audit
}

// CleanupGroupStates 使用逐行 CAS 在单一事务内删除显式群开关并写一条汇总审计。
func (r *PostgresRuntimeStateRepository) CleanupGroupStates(ctx context.Context, actor management.Actor, expected []RuntimeGroupStateRecord) ([]RuntimeGroupStateRecord, error) {
	if r == nil || r.database == nil {
		return nil, fmt.Errorf("插件状态仓库未初始化")
	}
	if err := validateRuntimeCleanupStates(expected); err != nil {
		return nil, err
	}
	sorted := append([]RuntimeGroupStateRecord(nil), expected...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].PluginKey == sorted[j].PluginKey {
			return sorted[i].GroupID < sorted[j].GroupID
		}
		return sorted[i].PluginKey < sorted[j].PluginKey
	})
	tx, err := r.database.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("开启过期群开关清理事务: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	locked := make([]RuntimeGroupStateRecord, 0, len(sorted))
	for _, item := range sorted {
		current := RuntimeGroupStateRecord{PluginKey: item.PluginKey, GroupID: item.GroupID}
		err = tx.QueryRow(ctx, `SELECT enabled,version,updated_at FROM plugin_group_states WHERE plugin_key=$1 AND group_id=$2 FOR UPDATE`, item.PluginKey, item.GroupID).
			Scan(&current.Enabled, &current.Version, &current.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && current.Version != item.Version {
			return nil, ErrRuntimeStateConflict
		}
		if err != nil {
			return nil, mapRuntimeBatchWriteError(fmt.Sprintf("锁定插件 %s 群 %d 状态", item.PluginKey, item.GroupID), err)
		}
		current.UpdatedAt = current.UpdatedAt.UTC()
		locked = append(locked, current)
	}
	for _, item := range locked {
		var deletedVersion int64
		err = tx.QueryRow(ctx, `DELETE FROM plugin_group_states WHERE plugin_key=$1 AND group_id=$2 AND version=$3 RETURNING version`, item.PluginKey, item.GroupID, item.Version).
			Scan(&deletedVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRuntimeStateConflict
		}
		if err != nil {
			return nil, mapRuntimeBatchWriteError("删除过期群开关", err)
		}
	}
	groups := make(map[int64]struct{}, len(locked))
	for _, item := range locked {
		groups[item.GroupID] = struct{}{}
	}
	targetID := "batch"
	if len(groups) == 1 {
		for groupID := range groups {
			targetID = fmt.Sprintf("%d", groupID)
		}
	}
	if err := recordRuntimeAudit(ctx, tx, actor, "plugin.runtime.group.cleanup", "plugin_runtime_group", targetID, runtimeGroupBatchAudit{States: locked}, runtimeGroupBatchAudit{States: []RuntimeGroupStateRecord{}}); err != nil {
		return nil, mapRuntimeBatchWriteError("写入过期群开关清理审计", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapRuntimeBatchCommitError("提交过期群开关清理事务", err)
	}
	return locked, nil
}

func validateRuntimeGroupCopyItems(sourceGroupID, targetGroupID int64, items []RuntimeGroupCopyItem) error {
	if sourceGroupID <= 0 || targetGroupID <= 0 {
		return ErrInvalidRuntimeGroupID
	}
	if sourceGroupID == targetGroupID {
		return ErrRuntimeGroupCopySameGroup
	}
	if len(items) == 0 {
		return ErrRuntimeGroupBatchEmpty
	}
	if len(items) > RuntimeGroupBatchLimit {
		return ErrRuntimeGroupBatchTooLarge
	}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if !identifierPattern.MatchString(item.PluginKey) {
			return fmt.Errorf("%w: %s", ErrRuntimePluginNotFound, item.PluginKey)
		}
		if item.SourceVersion <= 0 || item.TargetVersion < 0 {
			return ErrRuntimeStateInvalidVersion
		}
		if _, duplicate := seen[item.PluginKey]; duplicate {
			return ErrRuntimeGroupStateDuplicate
		}
		seen[item.PluginKey] = struct{}{}
	}
	return nil
}

func validateRuntimeAllowedPluginKeys(allowedPluginKeys []string, items []RuntimeGroupCopyItem) ([]string, error) {
	if len(allowedPluginKeys) == 0 {
		return nil, ErrRuntimePluginNotFound
	}
	allowed := make(map[string]struct{}, len(allowedPluginKeys))
	for _, pluginKey := range allowedPluginKeys {
		if !identifierPattern.MatchString(pluginKey) {
			return nil, fmt.Errorf("%w: %s", ErrRuntimePluginNotFound, pluginKey)
		}
		if _, duplicate := allowed[pluginKey]; duplicate {
			return nil, ErrRuntimeGroupStateDuplicate
		}
		allowed[pluginKey] = struct{}{}
	}
	for _, item := range items {
		if _, found := allowed[item.PluginKey]; !found {
			return nil, fmt.Errorf("%w: %s", ErrRuntimePluginNotFound, item.PluginKey)
		}
	}
	keys := append([]string(nil), allowedPluginKeys...)
	sort.Strings(keys)
	return keys, nil
}

func validateRuntimeCleanupStates(states []RuntimeGroupStateRecord) error {
	if len(states) == 0 {
		return ErrRuntimeGroupBatchEmpty
	}
	if len(states) > RuntimeGroupBatchLimit {
		return ErrRuntimeGroupBatchTooLarge
	}
	seen := make(map[RuntimeGroupStateKey]struct{}, len(states))
	for _, state := range states {
		if !identifierPattern.MatchString(state.PluginKey) {
			return fmt.Errorf("%w: %s", ErrRuntimePluginNotFound, state.PluginKey)
		}
		if state.GroupID <= 0 {
			return ErrInvalidRuntimeGroupID
		}
		if state.Version <= 0 {
			return ErrRuntimeStateInvalidVersion
		}
		key := RuntimeGroupStateKey{PluginKey: state.PluginKey, GroupID: state.GroupID}
		if _, duplicate := seen[key]; duplicate {
			return ErrRuntimeGroupStateDuplicate
		}
		seen[key] = struct{}{}
	}
	return nil
}

func mapRuntimeBatchWriteError(operation string, err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && (postgresError.Code == "23505" || postgresError.Code == "40001" || postgresError.Code == "40P01") {
		return ErrRuntimeStateConflict
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func mapRuntimeBatchCommitError(operation string, err error) error {
	mapped := mapRuntimeBatchWriteError(operation, err)
	if errors.Is(mapped, ErrRuntimeStateConflict) {
		return mapped
	}
	return errors.Join(ErrRuntimeStateCommitUnknown, mapped)
}

// recordRuntimeAudit 在状态写入所在事务追加审计；before 为 nil 表示新增。
func recordRuntimeAudit(ctx context.Context, tx pgx.Tx, actor management.Actor, action, targetType, targetID string, before, after any) error {
	var beforeJSON []byte
	if before != nil {
		encoded, err := json.Marshal(before)
		if err != nil {
			return fmt.Errorf("序列化 %s 审计旧值: %w", action, err)
		}
		beforeJSON = encoded
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		return fmt.Errorf("序列化 %s 审计新值: %w", action, err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO admin_audit_logs(actor_id,actor_role,channel,action,target_type,target_id,before_json,after_json,success,request_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,TRUE,NULLIF($9,''))`,
		actor.ID, actor.Role, actor.Channel, action, targetType, targetID, beforeJSON, afterJSON, actor.RequestID)
	// 审计失败必须回滚状态变更，避免出现无法追溯的开关操作。
	if err != nil {
		return fmt.Errorf("写入 %s 审计: %w", action, err)
	}
	return nil
}
