package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/w1ndys/w1ndys-bot/internal/management"
	"github.com/w1ndys/w1ndys-bot/internal/onebot"
)

const runtimeConfigCompensateTimeout = 10 * time.Second

// RuntimeStateStore 是插件开关应用服务依赖的持久化意图读写契约。
type RuntimeStateStore interface {
	LoadSnapshot(context.Context) ([]PersistedPluginState, error)
	FindState(context.Context, string) (PersistedPluginState, error)
	UpdateDesiredEnabled(context.Context, management.Actor, string, bool, int64) (PersistedPluginState, error)
	SetGroupEnabled(context.Context, management.Actor, string, int64, bool, int64) (PersistedGroupState, error)
	CopyGroupStates(context.Context, management.Actor, int64, int64, []string, []RuntimeGroupCopyItem) ([]RuntimeGroupStateRecord, error)
	CleanupGroupStates(context.Context, management.Actor, []RuntimeGroupStateRecord) ([]RuntimeGroupStateRecord, error)
	FindConfig(context.Context, string) (PersistedPluginConfig, error)
	SaveConfig(context.Context, management.Actor, string, ConfigSchema, json.RawMessage, int64) (PersistedPluginConfig, error)
}

var (
	ErrRuntimeConfigNotSupported        = errors.New("插件未声明小型配置")
	ErrRuntimeConfigInvalid             = errors.New("插件配置无效")
	ErrRuntimeConfigHookPanic           = errors.New("插件配置钩子发生 panic")
	ErrRuntimeGroupDirectoryUnavailable = errors.New("群目录不可用")
	ErrRuntimeGroupNotLive              = errors.New("群不在当前机器人群列表中")
	ErrRuntimeGroupCopySameGroup        = errors.New("源群和目标群不能相同")
	ErrRuntimeGroupBatchEmpty           = errors.New("群开关批次不能为空")
	ErrRuntimeGroupBatchTooLarge        = errors.New("群开关批次超过上限")
	ErrRuntimeGroupStateDuplicate       = errors.New("群开关批次包含重复记录")
	ErrRuntimeGroupStateNotStale        = errors.New("群开关记录不再过期")
	ErrRuntimeGroupSourceStateNotFound  = errors.New("源群没有显式插件开关")
)

// RuntimeGroupDirectory 提供 NapCat 当前群目录；维护操作始终传入 noCache=true。
type RuntimeGroupDirectory interface {
	GetGroupList(context.Context, bool) ([]onebot.GroupInfo, error)
}

// RuntimeGroupCopyPreview 是源、目标实时群信息和可执行 CAS 项。
type RuntimeGroupCopyPreview struct {
	Source onebot.GroupInfo       `json:"source"`
	Target onebot.GroupInfo       `json:"target"`
	States []RuntimeGroupCopyItem `json:"states"`
}

// RuntimeGroupCopyResult 是复制提交后的目标群权威记录。
type RuntimeGroupCopyResult struct {
	SourceGroupID int64                     `json:"source_group_id"`
	TargetGroupID int64                     `json:"target_group_id"`
	States        []RuntimeGroupStateRecord `json:"states"`
}

// RuntimeStaleGroupState 是数据库存在但 NapCat 实时群目录不存在的显式开关记录。
type RuntimeStaleGroupState = RuntimeGroupStateRecord

// RuntimeConfigView 是配置管理页需要的 Schema、脱敏值与乐观锁版本。
type RuntimeConfigView struct {
	PluginKey string          `json:"plugin_key"`
	Schema    ConfigSchema    `json:"schema"`
	Config    json.RawMessage `json:"config"`
	Version   int64           `json:"version"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// RuntimeAuthorizer 由平台管理服务实现，校验管理操作者身份与来源。
type RuntimeAuthorizer interface {
	Authorize(management.Actor) error
}

// RuntimeGroupView 是一个群开关的管理视图。
type RuntimeGroupView struct {
	GroupID   int64     `json:"group_id"`
	Enabled   bool      `json:"enabled"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RuntimeCommandView 是代码持有的命令声明，WebUI 只读展示，不接受修改。
type RuntimeCommandView struct {
	Key          string   `json:"key"`
	DisplayName  string   `json:"display_name"`
	Description  string   `json:"description"`
	Triggers     []string `json:"triggers"`
	Scope        string   `json:"scope"`
	AllowedRoles []string `json:"allowed_roles"`
}

// RuntimeStateView 同时暴露管理员意图与进程内实际运行状态，供 WebUI 展示两者分歧。
type RuntimeStateView struct {
	PluginKey      string               `json:"plugin_key"`
	DisplayName    string               `json:"display_name"`
	Description    string               `json:"description"`
	AdminPageKey   string               `json:"admin_page_key"`
	DesiredEnabled bool                 `json:"desired_enabled"`
	Version        int64                `json:"version"`
	UpdatedAt      time.Time            `json:"updated_at"`
	Status         RuntimeStatus        `json:"status"`
	InFlight       int                  `json:"in_flight"`
	LastError      string               `json:"last_error"`
	HasConfig      bool                 `json:"has_config"`
	Commands       []RuntimeCommandView `json:"commands"`
	Groups         []RuntimeGroupView   `json:"groups"`
}

// RuntimeService 是 WebUI 与 QQ 应急入口共用的插件开关唯一写路径。
//
// 持久化意图与内存运行状态按“更保守的一侧先执行”排序，两个方向都 fail-closed：
// 启用先落库再驱动生命周期，生命周期失败时运行时停在 failed 且不接流量；
// 关闭先停止内存准入再落库，落库失败时运行时已经关闭。
type RuntimeService struct {
	catalog    *SpecCatalog
	controller *RuntimeController
	store      RuntimeStateStore
	authorizer RuntimeAuthorizer
	groups     RuntimeGroupDirectory
	locks      sync.Map
}

// NewRuntimeService 创建插件开关应用服务。
func NewRuntimeService(catalog *SpecCatalog, controller *RuntimeController, store RuntimeStateStore, authorizer RuntimeAuthorizer, groups RuntimeGroupDirectory) (*RuntimeService, error) {
	if catalog == nil || controller == nil || isNilRuntimeDependency(store) || isNilRuntimeDependency(authorizer) || isNilRuntimeDependency(groups) {
		return nil, errors.New("插件开关服务依赖不能为空")
	}
	return &RuntimeService{catalog: catalog, controller: controller, store: store, authorizer: authorizer, groups: groups}, nil
}

// List 按编译期目录顺序返回全部插件的意图、运行状态和群开关。
func (s *RuntimeService) List(ctx context.Context, actor management.Actor) ([]RuntimeStateView, error) {
	if err := s.authorizer.Authorize(actor); err != nil {
		return nil, err
	}
	states, err := s.store.LoadSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取插件状态快照: %w", err)
	}
	persisted := make(map[string]PersistedPluginState, len(states))
	for _, state := range states {
		persisted[state.PluginKey] = state
	}
	specs := s.catalog.Specs()
	views := make([]RuntimeStateView, 0, len(specs))
	for _, spec := range specs {
		// 目录中存在但尚未同步出状态行的插件按默认关闭展示，不隐藏其存在。
		views = append(views, s.view(spec, persisted[spec.Key]))
	}
	return views, nil
}

// Get 返回单个插件的意图、运行状态和群开关。
func (s *RuntimeService) Get(ctx context.Context, actor management.Actor, pluginKey string) (RuntimeStateView, error) {
	if err := s.authorizer.Authorize(actor); err != nil {
		return RuntimeStateView{}, err
	}
	spec, err := s.spec(pluginKey)
	if err != nil {
		return RuntimeStateView{}, err
	}
	state, err := s.store.FindState(ctx, pluginKey)
	if err != nil {
		return RuntimeStateView{}, fmt.Errorf("读取插件 %s 状态: %w", pluginKey, err)
	}
	return s.view(spec, state), nil
}

// SetGlobalEnabled 按乐观锁修改全局启用意图并驱动生命周期。
// 已是目标意图时不产生写入与审计。
func (s *RuntimeService) SetGlobalEnabled(ctx context.Context, actor management.Actor, pluginKey string, enabled bool, expectedVersion int64) (RuntimeStateView, error) {
	if err := s.authorizer.Authorize(actor); err != nil {
		return RuntimeStateView{}, err
	}
	spec, err := s.spec(pluginKey)
	if err != nil {
		return RuntimeStateView{}, err
	}
	unlock := s.lock(pluginKey)
	defer unlock()

	current, err := s.store.FindState(ctx, pluginKey)
	if err != nil {
		return RuntimeStateView{}, fmt.Errorf("读取插件 %s 状态: %w", pluginKey, err)
	}
	if current.Version != expectedVersion {
		return RuntimeStateView{}, ErrRuntimeStateConflict
	}
	if current.DesiredEnabled == enabled {
		return s.view(spec, current), nil
	}
	// 生命周期无法接受本次切换时提前拒绝，避免把注定失败的意图写进数据库。
	if err := s.checkTransition(pluginKey, enabled); err != nil {
		return RuntimeStateView{}, err
	}
	if enabled {
		return s.enable(ctx, actor, spec, current)
	}
	return s.disable(ctx, actor, spec, current)
}

// SetGroupEnabled 按乐观锁修改单群开关；expectedVersion 为 0 表示尚无记录。
// 全局关闭时仍允许维护群意图，群开关由运行门禁在分发时判定。
func (s *RuntimeService) SetGroupEnabled(ctx context.Context, actor management.Actor, pluginKey string, groupID int64, enabled bool, expectedVersion int64) (RuntimeStateView, error) {
	if err := s.authorizer.Authorize(actor); err != nil {
		return RuntimeStateView{}, err
	}
	spec, err := s.spec(pluginKey)
	if err != nil {
		return RuntimeStateView{}, err
	}
	if groupID <= 0 {
		return RuntimeStateView{}, ErrInvalidRuntimeGroupID
	}
	unlock := s.lock(pluginKey)
	defer unlock()

	current, err := s.store.FindState(ctx, pluginKey)
	if err != nil {
		return RuntimeStateView{}, fmt.Errorf("读取插件 %s 状态: %w", pluginKey, err)
	}
	before, found := findPersistedGroup(current.Groups, groupID)
	switch {
	case found && before.Version != expectedVersion:
		return RuntimeStateView{}, ErrRuntimeStateConflict
	case !found && expectedVersion != 0:
		return RuntimeStateView{}, ErrRuntimeStateConflict
	case found && before.Enabled == enabled:
		return s.view(spec, current), nil
	}
	apply := func() error { return s.controller.SetGroupEnabled(pluginKey, groupID, enabled) }
	persist := func() (PersistedGroupState, error) {
		return s.store.SetGroupEnabled(ctx, actor, pluginKey, groupID, enabled, expectedVersion)
	}
	var saved PersistedGroupState
	if enabled {
		if saved, err = persist(); err != nil {
			return RuntimeStateView{}, fmt.Errorf("保存插件 %s 群 %d 状态: %w", pluginKey, groupID, err)
		}
		if err = apply(); err != nil {
			return RuntimeStateView{}, fmt.Errorf("应用插件 %s 群 %d 开关: %w", pluginKey, groupID, err)
		}
	} else {
		if err = apply(); err != nil {
			return RuntimeStateView{}, fmt.Errorf("应用插件 %s 群 %d 开关: %w", pluginKey, groupID, err)
		}
		if saved, err = persist(); err != nil {
			return RuntimeStateView{}, fmt.Errorf("保存插件 %s 群 %d 状态: %w", pluginKey, groupID, err)
		}
	}
	current.Groups = mergePersistedGroup(current.Groups, saved)
	return s.view(spec, current), nil
}

// PreviewGroupCopy 返回两个实时群及源群全部显式插件开关的 CAS 预览。
func (s *RuntimeService) PreviewGroupCopy(ctx context.Context, actor management.Actor, sourceGroupID, targetGroupID int64) (RuntimeGroupCopyPreview, error) {
	if err := s.authorizer.Authorize(actor); err != nil {
		return RuntimeGroupCopyPreview{}, err
	}
	live, err := s.liveGroups(ctx)
	if err != nil {
		return RuntimeGroupCopyPreview{}, err
	}
	source, target, err := validateLiveCopyGroups(live, sourceGroupID, targetGroupID)
	if err != nil {
		return RuntimeGroupCopyPreview{}, err
	}
	states, err := s.store.LoadSnapshot(ctx)
	if err != nil {
		return RuntimeGroupCopyPreview{}, fmt.Errorf("读取插件状态快照: %w", err)
	}
	items := s.groupCopyItems(states, sourceGroupID, targetGroupID)
	if len(items) == 0 {
		return RuntimeGroupCopyPreview{}, ErrRuntimeGroupSourceStateNotFound
	}
	if len(items) > RuntimeGroupBatchLimit {
		return RuntimeGroupCopyPreview{}, ErrRuntimeGroupBatchTooLarge
	}
	return RuntimeGroupCopyPreview{Source: source, Target: target, States: items}, nil
}

// CopyGroupStates 使用预览版本把源群显式插件开关合并复制到实时目标群。
func (s *RuntimeService) CopyGroupStates(ctx context.Context, actor management.Actor, sourceGroupID, targetGroupID int64, states []RuntimeGroupCopyItem) (RuntimeGroupCopyResult, error) {
	if err := s.authorizer.Authorize(actor); err != nil {
		return RuntimeGroupCopyResult{}, err
	}
	if err := validateRuntimeGroupCopyItems(sourceGroupID, targetGroupID, states); err != nil {
		return RuntimeGroupCopyResult{}, err
	}
	for _, state := range states {
		if _, err := s.spec(state.PluginKey); err != nil {
			return RuntimeGroupCopyResult{}, err
		}
	}
	live, err := s.liveGroups(ctx)
	if err != nil {
		return RuntimeGroupCopyResult{}, err
	}
	if _, _, err := validateLiveCopyGroups(live, sourceGroupID, targetGroupID); err != nil {
		return RuntimeGroupCopyResult{}, err
	}

	allPluginKeys := make([]string, 0, len(s.catalog.Specs()))
	for _, spec := range s.catalog.Specs() {
		allPluginKeys = append(allPluginKeys, spec.Key)
	}
	unlock := s.lockPlugins(allPluginKeys)
	defer unlock()

	snapshot, err := s.store.LoadSnapshot(ctx)
	if err != nil {
		return RuntimeGroupCopyResult{}, fmt.Errorf("读取插件状态快照: %w", err)
	}
	current := s.groupCopyItems(snapshot, sourceGroupID, targetGroupID)
	if len(current) == 0 {
		return RuntimeGroupCopyResult{}, ErrRuntimeGroupSourceStateNotFound
	}
	if !equalRuntimeGroupCopyCAS(current, states) {
		return RuntimeGroupCopyResult{}, ErrRuntimeStateConflict
	}
	copyPluginKeys := make([]string, 0, len(current))
	for _, state := range current {
		copyPluginKeys = append(copyPluginKeys, state.PluginKey)
	}
	previous, err := s.controller.snapshotGroupStates(targetGroupID, copyPluginKeys)
	if err != nil {
		return RuntimeGroupCopyResult{}, fmt.Errorf("读取目标群内存开关: %w", err)
	}

	falseStates := make(map[string]bool)
	for _, state := range current {
		if !state.Enabled {
			falseStates[state.PluginKey] = false
		}
	}
	if err := s.controller.SetGroupStates(targetGroupID, falseStates); err != nil {
		return RuntimeGroupCopyResult{}, fmt.Errorf("预关闭目标群开关: %w", err)
	}
	saved, err := s.store.CopyGroupStates(ctx, actor, sourceGroupID, targetGroupID, allPluginKeys, current)
	if err != nil {
		if errors.Is(err, ErrRuntimeStateCommitUnknown) {
			return RuntimeGroupCopyResult{}, s.closeAfterCopyCommit(targetGroupID, copyPluginKeys, fmt.Errorf("复制群开关: %w", err))
		}
		restoreErr := s.controller.restoreGroupStates(targetGroupID, previous)
		return RuntimeGroupCopyResult{}, errors.Join(fmt.Errorf("复制群开关: %w", err), restoreErr)
	}
	if len(saved) != len(current) {
		return RuntimeGroupCopyResult{}, s.closeAfterCopyCommit(targetGroupID, copyPluginKeys, errors.New("复制群开关返回记录数量不一致"))
	}
	expectedKeys := make(map[string]struct{}, len(current))
	expectedEnabled := make(map[string]bool, len(current))
	for _, state := range current {
		expectedKeys[state.PluginKey] = struct{}{}
		expectedEnabled[state.PluginKey] = state.Enabled
	}
	finalStates := make(map[string]bool, len(saved))
	for index := range saved {
		state := saved[index]
		if state.GroupID != targetGroupID {
			return RuntimeGroupCopyResult{}, s.closeAfterCopyCommit(targetGroupID, copyPluginKeys, errors.New("复制群开关返回了错误的目标群"))
		}
		if _, duplicate := finalStates[state.PluginKey]; duplicate {
			return RuntimeGroupCopyResult{}, s.closeAfterCopyCommit(targetGroupID, copyPluginKeys, errors.New("复制群开关返回了重复插件记录"))
		}
		if _, found := expectedKeys[state.PluginKey]; !found {
			return RuntimeGroupCopyResult{}, s.closeAfterCopyCommit(targetGroupID, copyPluginKeys, errors.New("复制群开关返回了未请求的插件记录"))
		}
		if state.Enabled != expectedEnabled[state.PluginKey] || state.Version <= 0 {
			return RuntimeGroupCopyResult{}, s.closeAfterCopyCommit(targetGroupID, copyPluginKeys, errors.New("复制群开关返回了无效状态"))
		}
		saved[index].UpdatedAt = state.UpdatedAt.UTC()
		finalStates[state.PluginKey] = state.Enabled
	}
	if err := s.controller.SetGroupStates(targetGroupID, finalStates); err != nil {
		return RuntimeGroupCopyResult{}, s.closeAfterCopyCommit(targetGroupID, copyPluginKeys, fmt.Errorf("发布目标群开关: %w", err))
	}
	sortRuntimeGroupStateRecords(saved)
	return RuntimeGroupCopyResult{SourceGroupID: sourceGroupID, TargetGroupID: targetGroupID, States: saved}, nil
}

// PreviewStaleGroupStates 返回数据库存在但实时群目录中不存在的显式开关。
func (s *RuntimeService) PreviewStaleGroupStates(ctx context.Context, actor management.Actor) ([]RuntimeStaleGroupState, error) {
	if err := s.authorizer.Authorize(actor); err != nil {
		return nil, err
	}
	live, err := s.liveGroups(ctx)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.store.LoadSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取插件状态快照: %w", err)
	}
	stale := make([]RuntimeStaleGroupState, 0)
	for _, state := range snapshot {
		for _, group := range state.Groups {
			if _, found := live[group.GroupID]; found {
				continue
			}
			stale = append(stale, RuntimeStaleGroupState{
				PluginKey: state.PluginKey,
				GroupID:   group.GroupID,
				Enabled:   group.Enabled,
				Version:   group.Version,
				UpdatedAt: group.UpdatedAt.UTC(),
			})
		}
	}
	sortRuntimeGroupStateRecords(stale)
	return stale, nil
}

// CleanupStaleGroupStates 重新确认群目录后，先关闭内存门禁，再以 CAS 批量删除过期记录。
func (s *RuntimeService) CleanupStaleGroupStates(ctx context.Context, actor management.Actor, states []RuntimeStaleGroupState) ([]RuntimeStaleGroupState, error) {
	if err := s.authorizer.Authorize(actor); err != nil {
		return nil, err
	}
	if err := validateRuntimeCleanupStates(states); err != nil {
		return nil, err
	}
	pluginKeys := make([]string, 0, len(states))
	runtimePluginKeys := make(map[string]struct{}, len(states))
	for _, state := range states {
		if _, found := s.catalog.Find(state.PluginKey); found {
			runtimePluginKeys[state.PluginKey] = struct{}{}
			pluginKeys = append(pluginKeys, state.PluginKey)
		}
	}
	live, err := s.liveGroups(ctx)
	if err != nil {
		return nil, err
	}
	for _, state := range states {
		if _, found := live[state.GroupID]; found {
			return nil, fmt.Errorf("%w: %d", ErrRuntimeGroupStateNotStale, state.GroupID)
		}
	}

	unlock := s.lockPlugins(pluginKeys)
	defer unlock()
	expected := make([]RuntimeGroupStateRecord, 0, len(states))
	closedByGroup := make(map[int64]map[string]bool)
	for _, state := range states {
		expected = append(expected, RuntimeGroupStateRecord{
			PluginKey: state.PluginKey,
			GroupID:   state.GroupID,
			Version:   state.Version,
		})
		if _, found := runtimePluginKeys[state.PluginKey]; found {
			if closedByGroup[state.GroupID] == nil {
				closedByGroup[state.GroupID] = make(map[string]bool)
			}
			closedByGroup[state.GroupID][state.PluginKey] = false
		}
	}
	sortRuntimeGroupStateRecords(expected)
	groupIDs := make([]int64, 0, len(closedByGroup))
	for groupID := range closedByGroup {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Slice(groupIDs, func(i, j int) bool { return groupIDs[i] < groupIDs[j] })
	for _, groupID := range groupIDs {
		if err := s.controller.SetGroupStates(groupID, closedByGroup[groupID]); err != nil {
			return nil, fmt.Errorf("预关闭过期群 %d 开关: %w", groupID, err)
		}
	}
	deleted, err := s.store.CleanupGroupStates(ctx, actor, expected)
	if err != nil {
		return nil, fmt.Errorf("清理过期群开关: %w", err)
	}
	if len(deleted) != len(expected) {
		return nil, errors.New("清理过期群开关返回记录数量不一致")
	}
	expectedVersions := make(map[RuntimeGroupStateKey]int64, len(expected))
	for _, state := range expected {
		expectedVersions[RuntimeGroupStateKey{PluginKey: state.PluginKey, GroupID: state.GroupID}] = state.Version
	}
	keys := make([]RuntimeGroupStateKey, 0, len(deleted))
	for index := range deleted {
		key := RuntimeGroupStateKey{PluginKey: deleted[index].PluginKey, GroupID: deleted[index].GroupID}
		version, found := expectedVersions[key]
		if !found || version != deleted[index].Version {
			return nil, errors.New("清理过期群开关返回了无效记录")
		}
		delete(expectedVersions, key)
		deleted[index].UpdatedAt = deleted[index].UpdatedAt.UTC()
		if _, found := runtimePluginKeys[deleted[index].PluginKey]; found {
			keys = append(keys, key)
		}
	}
	if err := s.controller.DeleteGroupStates(keys); err != nil {
		return nil, fmt.Errorf("删除过期群内存开关: %w", err)
	}
	sortRuntimeGroupStateRecords(deleted)
	return deleted, nil
}

// GetConfig 返回插件配置 Schema 与脱敏后的当前值。
func (s *RuntimeService) GetConfig(ctx context.Context, actor management.Actor, pluginKey string) (RuntimeConfigView, error) {
	if err := s.authorizer.Authorize(actor); err != nil {
		return RuntimeConfigView{}, err
	}
	spec, err := s.configurableSpec(pluginKey)
	if err != nil {
		return RuntimeConfigView{}, err
	}
	stored, err := s.store.FindConfig(ctx, pluginKey)
	if err != nil {
		return RuntimeConfigView{}, fmt.Errorf("读取插件 %s 配置: %w", pluginKey, err)
	}
	// 脱敏同时补齐 Schema 默认值，首次保存前缺失的字段也能正常渲染。
	redacted, err := RedactConfig(spec.Config.Schema, stored.ConfigJSON)
	if err != nil {
		return RuntimeConfigView{}, fmt.Errorf("%w: %v", ErrRuntimeConfigInvalid, err)
	}
	return RuntimeConfigView{
		PluginKey: pluginKey, Schema: spec.Config.Schema, Config: redacted,
		Version: stored.Version, UpdatedAt: stored.UpdatedAt.UTC(),
	}, nil
}

// SetConfig 按乐观锁合并、校验并保存配置，随后热应用到运行实例。
// 热应用失败会把数据库补偿回旧值，避免持久化配置与运行快照长期不一致。
func (s *RuntimeService) SetConfig(ctx context.Context, actor management.Actor, pluginKey string, update json.RawMessage, expectedVersion int64) (RuntimeConfigView, error) {
	if err := s.authorizer.Authorize(actor); err != nil {
		return RuntimeConfigView{}, err
	}
	spec, err := s.configurableSpec(pluginKey)
	if err != nil {
		return RuntimeConfigView{}, err
	}
	unlock := s.lock(pluginKey)
	defer unlock()

	current, err := s.store.FindConfig(ctx, pluginKey)
	if err != nil {
		return RuntimeConfigView{}, fmt.Errorf("读取插件 %s 配置: %w", pluginKey, err)
	}
	if current.Version != expectedVersion {
		return RuntimeConfigView{}, ErrRuntimeStateConflict
	}
	// 合并保留未提交的 secret 原值，随后严格规范化以拒绝未知字段和类型错误。
	merged, err := MergeConfigUpdate(spec.Config.Schema, current.ConfigJSON, update)
	if err != nil {
		return RuntimeConfigView{}, fmt.Errorf("%w: %v", ErrRuntimeConfigInvalid, err)
	}
	normalized, err := NormalizeConfig(spec.Config.Schema, merged)
	if err != nil {
		return RuntimeConfigView{}, fmt.Errorf("%w: %v", ErrRuntimeConfigInvalid, err)
	}
	if err := invokeConfigHook(ctx, spec.Config.Validate, normalized); err != nil {
		return RuntimeConfigView{}, fmt.Errorf("%w: %v", ErrRuntimeConfigInvalid, err)
	}
	saved, err := s.store.SaveConfig(ctx, actor, pluginKey, spec.Config.Schema, normalized, expectedVersion)
	if err != nil {
		return RuntimeConfigView{}, fmt.Errorf("保存插件 %s 配置: %w", pluginKey, err)
	}
	if applyErr := invokeConfigHook(ctx, spec.Config.Apply, normalized); applyErr != nil {
		return RuntimeConfigView{}, s.compensateConfig(ctx, actor, spec, current, saved, applyErr)
	}
	return s.configView(spec, saved)
}

// compensateConfig 在热应用失败后把数据库写回旧配置并重新发布旧快照。
func (s *RuntimeService) compensateConfig(ctx context.Context, actor management.Actor, spec PluginSpec, previous, saved PersistedPluginConfig, applyErr error) error {
	failure := fmt.Errorf("热应用插件 %s 配置: %w", spec.Key, applyErr)
	// 补偿必须脱离调用方取消信号，否则请求超时会留下已写库但未生效的配置。
	compensateContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeConfigCompensateTimeout)
	defer cancel()
	restored, err := s.store.SaveConfig(compensateContext, actor, spec.Key, spec.Config.Schema, previous.ConfigJSON, saved.Version)
	if err != nil {
		return errors.Join(failure, fmt.Errorf("补偿插件 %s 配置: %w", spec.Key, err))
	}
	// 旧值曾经成功应用过，重新发布可确保运行快照与补偿后的数据库一致。
	if err := invokeConfigHook(compensateContext, spec.Config.Apply, restored.ConfigJSON); err != nil {
		return errors.Join(failure, fmt.Errorf("恢复插件 %s 配置快照: %w", spec.Key, err))
	}
	return failure
}

// ApplyStoredConfig 在启动恢复期把持久化配置发布到运行实例。
func (s *RuntimeService) ApplyStoredConfig(ctx context.Context, spec PluginSpec, stored PersistedPluginConfig) error {
	if spec.Config == nil {
		return nil
	}
	normalized, err := NormalizeConfig(spec.Config.Schema, stored.ConfigJSON)
	if err != nil {
		return fmt.Errorf("%w: 插件 %s: %v", ErrRuntimeConfigInvalid, spec.Key, err)
	}
	if err := invokeConfigHook(ctx, spec.Config.Apply, normalized); err != nil {
		return fmt.Errorf("热应用插件 %s 配置: %w", spec.Key, err)
	}
	return nil
}

func (s *RuntimeService) configView(spec PluginSpec, stored PersistedPluginConfig) (RuntimeConfigView, error) {
	redacted, err := RedactConfig(spec.Config.Schema, stored.ConfigJSON)
	if err != nil {
		return RuntimeConfigView{}, fmt.Errorf("%w: %v", ErrRuntimeConfigInvalid, err)
	}
	return RuntimeConfigView{
		PluginKey: spec.Key, Schema: spec.Config.Schema, Config: redacted,
		Version: stored.Version, UpdatedAt: stored.UpdatedAt.UTC(),
	}, nil
}

func (s *RuntimeService) configurableSpec(pluginKey string) (PluginSpec, error) {
	spec, err := s.spec(pluginKey)
	if err != nil {
		return PluginSpec{}, err
	}
	// 未声明配置的插件没有该子资源，不应伪装成空 Schema。
	if spec.Config == nil {
		return PluginSpec{}, fmt.Errorf("%w: %s", ErrRuntimeConfigNotSupported, pluginKey)
	}
	return spec, nil
}

// invokeConfigHook 调用插件配置钩子并隔离 panic。
func invokeConfigHook(ctx context.Context, hook func(context.Context, json.RawMessage) error, raw json.RawMessage) (err error) {
	if hook == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			err = ErrRuntimeConfigHookPanic
		}
	}()
	return hook(ctx, raw)
}

// enable 先持久化启用意图，再执行生命周期；生命周期失败不回滚意图，由运行状态暴露分歧。
func (s *RuntimeService) enable(ctx context.Context, actor management.Actor, spec PluginSpec, current PersistedPluginState) (RuntimeStateView, error) {
	saved, err := s.store.UpdateDesiredEnabled(ctx, actor, spec.Key, true, current.Version)
	if err != nil {
		return RuntimeStateView{}, fmt.Errorf("保存插件 %s 启用意图: %w", spec.Key, err)
	}
	saved.Groups = current.Groups
	if err := s.controller.Enable(ctx, spec.Key); err != nil {
		return s.view(spec, saved), fmt.Errorf("启用插件 %s 运行时: %w", spec.Key, err)
	}
	return s.view(spec, saved), nil
}

// disable 先停止内存准入并排空在途调用，再持久化关闭意图。
func (s *RuntimeService) disable(ctx context.Context, actor management.Actor, spec PluginSpec, current PersistedPluginState) (RuntimeStateView, error) {
	// 生命周期清理失败时运行时已停止准入，仍需落库关闭意图，避免重启后复活管理员已关停的插件。
	disableErr := s.controller.Disable(ctx, spec.Key)
	saved, err := s.store.UpdateDesiredEnabled(ctx, actor, spec.Key, false, current.Version)
	if err != nil {
		return RuntimeStateView{}, errors.Join(fmt.Errorf("保存插件 %s 关闭意图: %w", spec.Key, err), disableErr)
	}
	saved.Groups = current.Groups
	if disableErr != nil {
		return s.view(spec, saved), fmt.Errorf("关闭插件 %s 运行时: %w", spec.Key, disableErr)
	}
	return s.view(spec, saved), nil
}

// checkTransition 在写库前判断生命周期是否可以接受本次切换。
func (s *RuntimeService) checkTransition(pluginKey string, enabled bool) error {
	state, found := s.controller.State(pluginKey)
	if !found {
		return ErrRuntimePluginNotFound
	}
	switch state.Status {
	case RuntimeEnabling, RuntimeDisabling:
		return ErrRuntimeTransition
	case RuntimeFailed:
		// 故障插件必须先关闭完成清理，再重新启用。
		if enabled {
			return ErrRuntimeRecoveryNeeded
		}
	}
	return nil
}

func (s *RuntimeService) spec(pluginKey string) (PluginSpec, error) {
	spec, found := s.catalog.Find(pluginKey)
	if !found {
		return PluginSpec{}, fmt.Errorf("%w: %s", ErrRuntimePluginNotFound, pluginKey)
	}
	return spec, nil
}

func (s *RuntimeService) lock(pluginKey string) func() {
	value, _ := s.locks.LoadOrStore(pluginKey, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func (s *RuntimeService) lockPlugins(pluginKeys []string) func() {
	keys := append([]string(nil), pluginKeys...)
	sort.Strings(keys)
	unlockers := make([]func(), 0, len(keys))
	previous := ""
	for _, pluginKey := range keys {
		if pluginKey == previous {
			continue
		}
		unlockers = append(unlockers, s.lock(pluginKey))
		previous = pluginKey
	}
	return func() {
		for index := len(unlockers) - 1; index >= 0; index-- {
			unlockers[index]()
		}
	}
}

func (s *RuntimeService) closeAfterCopyCommit(groupID int64, pluginKeys []string, cause error) error {
	closed := make(map[string]bool, len(pluginKeys))
	for _, pluginKey := range pluginKeys {
		closed[pluginKey] = false
	}
	if err := s.controller.SetGroupStates(groupID, closed); err != nil {
		return errors.Join(cause, fmt.Errorf("提交后关闭目标群开关: %w", err))
	}
	return cause
}

func (s *RuntimeService) liveGroups(ctx context.Context) (map[int64]onebot.GroupInfo, error) {
	if s.groups == nil {
		return nil, ErrRuntimeGroupDirectoryUnavailable
	}
	groups, err := s.groups.GetGroupList(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRuntimeGroupDirectoryUnavailable, err)
	}
	live := make(map[int64]onebot.GroupInfo, len(groups))
	for _, group := range groups {
		live[group.GroupID] = group
	}
	return live, nil
}

func validateLiveCopyGroups(live map[int64]onebot.GroupInfo, sourceGroupID, targetGroupID int64) (onebot.GroupInfo, onebot.GroupInfo, error) {
	if sourceGroupID <= 0 || targetGroupID <= 0 {
		return onebot.GroupInfo{}, onebot.GroupInfo{}, ErrInvalidRuntimeGroupID
	}
	if sourceGroupID == targetGroupID {
		return onebot.GroupInfo{}, onebot.GroupInfo{}, ErrRuntimeGroupCopySameGroup
	}
	source, sourceFound := live[sourceGroupID]
	target, targetFound := live[targetGroupID]
	if !sourceFound {
		return onebot.GroupInfo{}, onebot.GroupInfo{}, fmt.Errorf("%w: %d", ErrRuntimeGroupNotLive, sourceGroupID)
	}
	if !targetFound {
		return onebot.GroupInfo{}, onebot.GroupInfo{}, fmt.Errorf("%w: %d", ErrRuntimeGroupNotLive, targetGroupID)
	}
	return source, target, nil
}

func (s *RuntimeService) groupCopyItems(snapshot []PersistedPluginState, sourceGroupID, targetGroupID int64) []RuntimeGroupCopyItem {
	persisted := make(map[string]PersistedPluginState, len(snapshot))
	for _, state := range snapshot {
		persisted[state.PluginKey] = state
	}
	items := make([]RuntimeGroupCopyItem, 0)
	for _, spec := range s.catalog.Specs() {
		state := persisted[spec.Key]
		source, found := findPersistedGroup(state.Groups, sourceGroupID)
		if !found {
			continue
		}
		item := RuntimeGroupCopyItem{
			PluginKey:     spec.Key,
			Enabled:       source.Enabled,
			SourceVersion: source.Version,
		}
		if target, targetFound := findPersistedGroup(state.Groups, targetGroupID); targetFound {
			item.TargetVersion = target.Version
			targetEnabled := target.Enabled
			item.TargetEnabled = &targetEnabled
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].PluginKey < items[j].PluginKey })
	return items
}

func equalRuntimeGroupCopyCAS(current, requested []RuntimeGroupCopyItem) bool {
	if len(current) != len(requested) {
		return false
	}
	byKey := make(map[string]RuntimeGroupCopyItem, len(requested))
	for _, item := range requested {
		if _, duplicate := byKey[item.PluginKey]; duplicate {
			return false
		}
		byKey[item.PluginKey] = item
	}
	for _, item := range current {
		requestedItem, found := byKey[item.PluginKey]
		if !found || requestedItem.Enabled != item.Enabled || requestedItem.SourceVersion != item.SourceVersion || requestedItem.TargetVersion != item.TargetVersion {
			return false
		}
	}
	return true
}

func sortRuntimeGroupStateRecords(states []RuntimeGroupStateRecord) {
	sort.Slice(states, func(i, j int) bool {
		if states[i].PluginKey == states[j].PluginKey {
			return states[i].GroupID < states[j].GroupID
		}
		return states[i].PluginKey < states[j].PluginKey
	})
}

func (s *RuntimeService) view(spec PluginSpec, state PersistedPluginState) RuntimeStateView {
	view := RuntimeStateView{
		PluginKey:      spec.Key,
		DisplayName:    spec.DisplayName,
		Description:    spec.Description,
		AdminPageKey:   spec.AdminPageKey,
		DesiredEnabled: state.DesiredEnabled,
		Version:        state.Version,
		UpdatedAt:      state.UpdatedAt.UTC(),
		Status:         RuntimeDisabled,
		HasConfig:      spec.Config != nil,
		Commands:       make([]RuntimeCommandView, 0, len(spec.Commands)),
		Groups:         make([]RuntimeGroupView, 0, len(state.Groups)),
	}
	for _, command := range spec.Commands {
		roles := make([]string, 0, len(command.AllowedRoles))
		for role := range command.AllowedRoles {
			roles = append(roles, string(role))
		}
		// 身份集合来自映射，排序后输出使 API 响应稳定可比对。
		sort.Strings(roles)
		view.Commands = append(view.Commands, RuntimeCommandView{
			Key: command.Key, DisplayName: command.DisplayName, Description: command.Description,
			Triggers: append([]string{}, command.Triggers...), Scope: string(command.Scope), AllowedRoles: roles,
		})
	}
	if runtimeState, found := s.controller.State(spec.Key); found {
		view.Status, view.InFlight = runtimeState.Status, runtimeState.InFlight
		if runtimeState.LastError != nil {
			view.LastError = runtimeState.LastError.Error()
		}
	}
	for _, group := range state.Groups {
		view.Groups = append(view.Groups, RuntimeGroupView{
			GroupID: group.GroupID, Enabled: group.Enabled, Version: group.Version, UpdatedAt: group.UpdatedAt.UTC(),
		})
	}
	return view
}

func findPersistedGroup(groups []PersistedGroupState, groupID int64) (PersistedGroupState, bool) {
	for _, group := range groups {
		if group.GroupID == groupID {
			return group, true
		}
	}
	return PersistedGroupState{}, false
}

func mergePersistedGroup(groups []PersistedGroupState, saved PersistedGroupState) []PersistedGroupState {
	merged := make([]PersistedGroupState, 0, len(groups)+1)
	replaced := false
	for _, group := range groups {
		if group.GroupID == saved.GroupID {
			merged, replaced = append(merged, saved), true
			continue
		}
		merged = append(merged, group)
	}
	if !replaced {
		merged = append(merged, saved)
	}
	sort.Slice(merged, func(first, second int) bool { return merged[first].GroupID < merged[second].GroupID })
	return merged
}

func isNilRuntimeDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
