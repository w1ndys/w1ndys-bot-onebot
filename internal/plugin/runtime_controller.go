package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// RuntimeStatus 表示插件当前进程内的实际运行状态。
type RuntimeStatus string

const (
	RuntimeDisabled  RuntimeStatus = "disabled"
	RuntimeEnabling  RuntimeStatus = "enabling"
	RuntimeReady     RuntimeStatus = "ready"
	RuntimeDisabling RuntimeStatus = "disabling"
	RuntimeFailed    RuntimeStatus = "failed"
)

var (
	ErrRuntimePluginNotFound = errors.New("运行时插件不存在")
	ErrRuntimeTransition     = errors.New("插件正在切换运行状态")
	ErrRuntimeRecoveryNeeded = errors.New("插件故障后必须先禁用清理")
	ErrInvalidRuntimeGroupID = errors.New("群号必须大于零")
	ErrRuntimeLifecyclePanic = errors.New("插件生命周期发生 panic")
)

// RuntimeState 是供后续管理服务读取的进程内状态快照。
type RuntimeState struct {
	Status    RuntimeStatus
	InFlight  int
	LastError error
}

type runtimeEntry struct {
	mu        sync.Mutex
	status    RuntimeStatus
	lifecycle Lifecycle
	groups    map[int64]bool
	inFlight  int
	idle      chan struct{}
	lastError error
}

type runtimeGroupMemoryState struct {
	Enabled bool
	Exists  bool
}

// RuntimeController 管理目标插件的纯内存生命周期、群门禁和在途调用。
type RuntimeController struct {
	entries map[string]*runtimeEntry
}

// NewRuntimeController 从编译期目录建立默认关闭的运行条目。
func NewRuntimeController(catalog *SpecCatalog) (*RuntimeController, error) {
	if catalog == nil {
		return nil, errors.New("运行时规格目录不能为空")
	}
	entries := make(map[string]*runtimeEntry)
	for _, spec := range catalog.Specs() {
		entries[spec.Key] = &runtimeEntry{
			status:    RuntimeDisabled,
			lifecycle: spec.Lifecycle,
			groups:    make(map[int64]bool),
		}
	}
	return &RuntimeController{entries: entries}, nil
}

// State 返回指定插件的运行状态快照。
func (c *RuntimeController) State(pluginKey string) (RuntimeState, bool) {
	entry, found := c.entry(pluginKey)
	if !found {
		return RuntimeState{}, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return RuntimeState{Status: entry.status, InFlight: entry.inFlight, LastError: entry.lastError}, true
}

// SetGroupEnabled 更新纯内存群开关；插件关闭时仍允许管理该意图。
func (c *RuntimeController) SetGroupEnabled(pluginKey string, groupID int64, enabled bool) error {
	if groupID <= 0 {
		return ErrInvalidRuntimeGroupID
	}
	entry, found := c.entry(pluginKey)
	if !found {
		return ErrRuntimePluginNotFound
	}
	entry.mu.Lock()
	entry.groups[groupID] = enabled
	entry.mu.Unlock()
	return nil
}

// SetGroupStates 在预校验后按固定锁顺序原子发布一个群的多个插件门禁。
func (c *RuntimeController) SetGroupStates(groupID int64, states map[string]bool) error {
	if groupID <= 0 {
		return ErrInvalidRuntimeGroupID
	}
	keys := make([]string, 0, len(states))
	entries := make([]*runtimeEntry, 0, len(states))
	for pluginKey := range states {
		if _, found := c.entry(pluginKey); !found {
			return fmt.Errorf("%w: %s", ErrRuntimePluginNotFound, pluginKey)
		}
		keys = append(keys, pluginKey)
	}
	sort.Strings(keys)
	for _, pluginKey := range keys {
		entry, _ := c.entry(pluginKey)
		entry.mu.Lock()
		entries = append(entries, entry)
	}
	for index, pluginKey := range keys {
		entries[index].groups[groupID] = states[pluginKey]
	}
	for index := len(entries) - 1; index >= 0; index-- {
		entries[index].mu.Unlock()
	}
	return nil
}

// DeleteGroupStates 在预校验后按固定锁顺序删除多个显式群门禁；缺失记录保持幂等。
func (c *RuntimeController) DeleteGroupStates(states []RuntimeGroupStateKey) error {
	keys := make([]RuntimeGroupStateKey, len(states))
	copy(keys, states)
	seen := make(map[RuntimeGroupStateKey]struct{}, len(keys))
	for _, state := range keys {
		if state.GroupID <= 0 {
			return ErrInvalidRuntimeGroupID
		}
		if _, found := c.entry(state.PluginKey); !found {
			return fmt.Errorf("%w: %s", ErrRuntimePluginNotFound, state.PluginKey)
		}
		if _, duplicate := seen[state]; duplicate {
			return ErrRuntimeGroupStateDuplicate
		}
		seen[state] = struct{}{}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].PluginKey == keys[j].PluginKey {
			return keys[i].GroupID < keys[j].GroupID
		}
		return keys[i].PluginKey < keys[j].PluginKey
	})
	locked := make([]*runtimeEntry, 0, len(keys))
	lastPluginKey := ""
	for _, state := range keys {
		if state.PluginKey == lastPluginKey {
			continue
		}
		entry, _ := c.entry(state.PluginKey)
		entry.mu.Lock()
		locked = append(locked, entry)
		lastPluginKey = state.PluginKey
	}
	for _, state := range keys {
		entry, _ := c.entry(state.PluginKey)
		delete(entry.groups, state.GroupID)
	}
	for index := len(locked) - 1; index >= 0; index-- {
		locked[index].mu.Unlock()
	}
	return nil
}

func (c *RuntimeController) snapshotGroupStates(groupID int64, pluginKeys []string) (map[string]runtimeGroupMemoryState, error) {
	if groupID <= 0 {
		return nil, ErrInvalidRuntimeGroupID
	}
	keys := append([]string(nil), pluginKeys...)
	sort.Strings(keys)
	entries := make([]*runtimeEntry, 0, len(keys))
	previous := ""
	for _, pluginKey := range keys {
		if pluginKey == previous {
			return nil, ErrRuntimeGroupStateDuplicate
		}
		entry, found := c.entry(pluginKey)
		if !found {
			return nil, fmt.Errorf("%w: %s", ErrRuntimePluginNotFound, pluginKey)
		}
		entries = append(entries, entry)
		previous = pluginKey
	}
	for _, entry := range entries {
		entry.mu.Lock()
	}
	states := make(map[string]runtimeGroupMemoryState, len(keys))
	for index, pluginKey := range keys {
		enabled, found := entries[index].groups[groupID]
		states[pluginKey] = runtimeGroupMemoryState{Enabled: enabled, Exists: found}
	}
	for index := len(entries) - 1; index >= 0; index-- {
		entries[index].mu.Unlock()
	}
	return states, nil
}

func (c *RuntimeController) restoreGroupStates(groupID int64, states map[string]runtimeGroupMemoryState) error {
	if groupID <= 0 {
		return ErrInvalidRuntimeGroupID
	}
	keys := make([]string, 0, len(states))
	entries := make([]*runtimeEntry, 0, len(states))
	for pluginKey := range states {
		if _, found := c.entry(pluginKey); !found {
			return fmt.Errorf("%w: %s", ErrRuntimePluginNotFound, pluginKey)
		}
		keys = append(keys, pluginKey)
	}
	sort.Strings(keys)
	for _, pluginKey := range keys {
		entry, _ := c.entry(pluginKey)
		entry.mu.Lock()
		entries = append(entries, entry)
	}
	for index, pluginKey := range keys {
		state := states[pluginKey]
		if state.Exists {
			entries[index].groups[groupID] = state.Enabled
		} else {
			delete(entries[index].groups, groupID)
		}
	}
	for index := len(entries) - 1; index >= 0; index-- {
		entries[index].mu.Unlock()
	}
	return nil
}

func (c *RuntimeController) replaceGroupStates(pluginKey string, groups map[int64]bool) error {
	entry, found := c.entry(pluginKey)
	if !found {
		return ErrRuntimePluginNotFound
	}
	next := make(map[int64]bool, len(groups))
	for groupID, enabled := range groups {
		if groupID <= 0 {
			return ErrInvalidRuntimeGroupID
		}
		next[groupID] = enabled
	}
	entry.mu.Lock()
	entry.groups = next
	entry.mu.Unlock()
	return nil
}

// Enable 完成生命周期准备后才把插件发布为 Ready。
func (c *RuntimeController) Enable(ctx context.Context, pluginKey string) error {
	entry, found := c.entry(pluginKey)
	if !found {
		return ErrRuntimePluginNotFound
	}
	entry.mu.Lock()
	switch entry.status {
	case RuntimeReady:
		entry.mu.Unlock()
		return nil
	case RuntimeEnabling, RuntimeDisabling:
		entry.mu.Unlock()
		return ErrRuntimeTransition
	case RuntimeFailed:
		entry.mu.Unlock()
		return ErrRuntimeRecoveryNeeded
	}
	entry.status = RuntimeEnabling
	entry.lastError = nil
	entry.mu.Unlock()

	err := invokeLifecycle(ctx, entry.lifecycle, true)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if err != nil {
		entry.status = RuntimeFailed
		entry.lastError = err
		return fmt.Errorf("启用插件 %s: %w", pluginKey, err)
	}
	entry.status = RuntimeReady
	return nil
}

// Disable 停止新准入，排空在途调用后执行资源清理。
func (c *RuntimeController) Disable(ctx context.Context, pluginKey string) error {
	entry, found := c.entry(pluginKey)
	if !found {
		return ErrRuntimePluginNotFound
	}
	entry.mu.Lock()
	switch entry.status {
	case RuntimeDisabled:
		entry.mu.Unlock()
		return nil
	case RuntimeEnabling, RuntimeDisabling:
		entry.mu.Unlock()
		return ErrRuntimeTransition
	}
	entry.status = RuntimeDisabling
	waitForIdle := entry.idle
	entry.mu.Unlock()

	if waitForIdle != nil {
		select {
		case <-waitForIdle:
		case <-ctx.Done():
			return c.failTransition(entry, fmt.Errorf("等待插件 %s 在途调用排空: %w", pluginKey, ctx.Err()))
		}
	}
	err := invokeLifecycle(ctx, entry.lifecycle, false)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if err != nil {
		entry.status = RuntimeFailed
		entry.lastError = err
		return fmt.Errorf("禁用插件 %s: %w", pluginKey, err)
	}
	entry.status = RuntimeDisabled
	entry.lastError = nil
	return nil
}

// Admit 仅为 Ready 插件原子登记一次在途调用。
func (c *RuntimeController) Admit(pluginKey string) (Admission, bool) {
	entry, found := c.entry(pluginKey)
	if !found {
		return nil, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.status != RuntimeReady {
		return nil, false
	}
	if entry.inFlight == 0 {
		entry.idle = make(chan struct{})
	}
	entry.inFlight++
	return &runtimeAdmission{entry: entry}, true
}

func (c *RuntimeController) entry(pluginKey string) (*runtimeEntry, bool) {
	if c == nil {
		return nil, false
	}
	entry, found := c.entries[pluginKey]
	return entry, found
}

func (c *RuntimeController) failTransition(entry *runtimeEntry, err error) error {
	entry.mu.Lock()
	entry.status = RuntimeFailed
	entry.lastError = err
	entry.mu.Unlock()
	return err
}

type runtimeAdmission struct {
	entry    *runtimeEntry
	released bool
}

// GroupEnabled 查询本次 admission 所属插件的群开关快照。
func (a *runtimeAdmission) GroupEnabled(groupID int64) bool {
	if a == nil || a.entry == nil || groupID <= 0 {
		return false
	}
	a.entry.mu.Lock()
	enabled := !a.released && a.entry.groups[groupID]
	a.entry.mu.Unlock()
	return enabled
}

// Release 幂等释放一次在途调用，并通知等待排空的禁用流程。
func (a *runtimeAdmission) Release() {
	if a == nil || a.entry == nil {
		return
	}
	a.entry.mu.Lock()
	if a.released {
		a.entry.mu.Unlock()
		return
	}
	a.released = true
	a.entry.inFlight--
	if a.entry.inFlight == 0 {
		close(a.entry.idle)
		a.entry.idle = nil
	}
	a.entry.mu.Unlock()
}

func invokeLifecycle(ctx context.Context, lifecycle Lifecycle, enable bool) (err error) {
	if lifecycle == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			err = ErrRuntimeLifecyclePanic
		}
	}()
	if enable {
		return lifecycle.OnEnable(ctx)
	}
	return lifecycle.OnDisable(ctx)
}
