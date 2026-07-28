package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w1ndys/w1ndys-bot/internal/management"
	"github.com/w1ndys/w1ndys-bot/internal/onebot"
)

var runtimeServiceActor = management.Actor{ID: "10001", Role: "super_admin", Channel: management.ChannelWebUI, RequestID: "req-1"}

// runtimeServiceStore 是记录调用顺序和操作者的内存状态仓库。
type runtimeServiceStore struct {
	mu            sync.Mutex
	states        map[string]PersistedPluginState
	configs       map[string]PersistedPluginConfig
	events        []string
	actors        []management.Actor
	loadErr       error
	findErr       error
	updateErr     error
	groupErr      error
	copyErr       error
	cleanupErr    error
	copyResult    []RuntimeGroupStateRecord
	beforeCopy    func()
	beforeCleanup func()
	findConfigErr error
	saveConfigErr error
}

func (s *runtimeServiceStore) FindConfig(_ context.Context, pluginKey string) (PersistedPluginConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findConfigErr != nil {
		return PersistedPluginConfig{}, s.findConfigErr
	}
	config, found := s.configs[pluginKey]
	if !found {
		return PersistedPluginConfig{}, ErrRuntimeConfigNotFound
	}
	return config, nil
}

func (s *runtimeServiceStore) SaveConfig(_ context.Context, actor management.Actor, pluginKey string, _ ConfigSchema, configJSON json.RawMessage, expectedVersion int64) (PersistedPluginConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "persist-config")
	s.actors = append(s.actors, actor)
	if s.saveConfigErr != nil {
		return PersistedPluginConfig{}, s.saveConfigErr
	}
	config, found := s.configs[pluginKey]
	if !found {
		return PersistedPluginConfig{}, ErrRuntimeConfigNotFound
	}
	if config.Version != expectedVersion {
		return PersistedPluginConfig{}, ErrRuntimeStateConflict
	}
	config.ConfigJSON, config.Version, config.UpdatedAt = append(json.RawMessage(nil), configJSON...), config.Version+1, time.Now().UTC()
	s.configs[pluginKey] = config
	return config, nil
}

func newRuntimeServiceStore(states ...PersistedPluginState) *runtimeServiceStore {
	store := &runtimeServiceStore{states: make(map[string]PersistedPluginState, len(states)), configs: make(map[string]PersistedPluginConfig)}
	for _, state := range states {
		if state.Groups == nil {
			state.Groups = make([]PersistedGroupState, 0)
		}
		store.states[state.PluginKey] = state
	}
	return store
}

func (s *runtimeServiceStore) LoadSnapshot(context.Context) ([]PersistedPluginState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	states := make([]PersistedPluginState, 0, len(s.states))
	for _, state := range s.states {
		states = append(states, state)
	}
	sort.Slice(states, func(first, second int) bool { return states[first].PluginKey < states[second].PluginKey })
	return states, nil
}

func (s *runtimeServiceStore) FindState(_ context.Context, pluginKey string) (PersistedPluginState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findErr != nil {
		return PersistedPluginState{}, s.findErr
	}
	state, found := s.states[pluginKey]
	if !found {
		return PersistedPluginState{}, ErrRuntimeStateNotFound
	}
	state.Groups = append(make([]PersistedGroupState, 0, len(state.Groups)), state.Groups...)
	return state, nil
}

func (s *runtimeServiceStore) UpdateDesiredEnabled(_ context.Context, actor management.Actor, pluginKey string, enabled bool, expectedVersion int64) (PersistedPluginState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "persist")
	s.actors = append(s.actors, actor)
	if s.updateErr != nil {
		return PersistedPluginState{}, s.updateErr
	}
	state, found := s.states[pluginKey]
	if !found {
		return PersistedPluginState{}, ErrRuntimeStateNotFound
	}
	if state.Version != expectedVersion {
		return PersistedPluginState{}, ErrRuntimeStateConflict
	}
	state.DesiredEnabled, state.Version, state.UpdatedAt = enabled, state.Version+1, time.Now().UTC()
	s.states[pluginKey] = state
	return PersistedPluginState{PluginKey: pluginKey, DesiredEnabled: enabled, Version: state.Version, UpdatedAt: state.UpdatedAt}, nil
}

func (s *runtimeServiceStore) SetGroupEnabled(_ context.Context, actor management.Actor, pluginKey string, groupID int64, enabled bool, expectedVersion int64) (PersistedGroupState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "persist-group")
	s.actors = append(s.actors, actor)
	if s.groupErr != nil {
		return PersistedGroupState{}, s.groupErr
	}
	state := s.states[pluginKey]
	saved := PersistedGroupState{GroupID: groupID, Enabled: enabled, Version: expectedVersion + 1, UpdatedAt: time.Now().UTC()}
	state.Groups = append(state.Groups, saved)
	s.states[pluginKey] = state
	return saved, nil
}

func (s *runtimeServiceStore) CopyGroupStates(_ context.Context, actor management.Actor, sourceGroupID, targetGroupID int64, allowedPluginKeys []string, items []RuntimeGroupCopyItem) ([]RuntimeGroupStateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "persist-copy")
	s.actors = append(s.actors, actor)
	if s.beforeCopy != nil {
		s.beforeCopy()
	}
	if s.copyErr != nil {
		return nil, s.copyErr
	}
	requested := make(map[string]struct{}, len(items))
	for _, item := range items {
		requested[item.PluginKey] = struct{}{}
	}
	for _, pluginKey := range allowedPluginKeys {
		if source, found := findPersistedGroup(s.states[pluginKey].Groups, sourceGroupID); found {
			if _, included := requested[pluginKey]; !included || source.Version <= 0 {
				return nil, ErrRuntimeStateConflict
			}
		}
	}
	now := time.Now().UTC()
	saved := make([]RuntimeGroupStateRecord, 0, len(items))
	for _, item := range items {
		state, found := s.states[item.PluginKey]
		if !found {
			return nil, ErrRuntimeStateConflict
		}
		source, sourceFound := findPersistedGroup(state.Groups, sourceGroupID)
		target, targetFound := findPersistedGroup(state.Groups, targetGroupID)
		if !sourceFound || source.Enabled != item.Enabled || source.Version != item.SourceVersion || targetFound && target.Version != item.TargetVersion || !targetFound && item.TargetVersion != 0 {
			return nil, ErrRuntimeStateConflict
		}
		if targetFound && target.Enabled == item.Enabled {
			saved = append(saved, RuntimeGroupStateRecord{PluginKey: item.PluginKey, GroupID: targetGroupID, Enabled: target.Enabled, Version: target.Version, UpdatedAt: target.UpdatedAt})
			continue
		}
		version := int64(1)
		if targetFound {
			version = target.Version + 1
		}
		target = PersistedGroupState{GroupID: targetGroupID, Enabled: item.Enabled, Version: version, UpdatedAt: now}
		state.Groups = mergePersistedGroup(state.Groups, target)
		s.states[item.PluginKey] = state
		saved = append(saved, RuntimeGroupStateRecord{PluginKey: item.PluginKey, GroupID: targetGroupID, Enabled: item.Enabled, Version: version, UpdatedAt: now})
	}
	if s.copyResult != nil {
		return append([]RuntimeGroupStateRecord(nil), s.copyResult...), nil
	}
	return saved, nil
}

func (s *runtimeServiceStore) CleanupGroupStates(_ context.Context, actor management.Actor, expected []RuntimeGroupStateRecord) ([]RuntimeGroupStateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "persist-cleanup")
	s.actors = append(s.actors, actor)
	if s.beforeCleanup != nil {
		s.beforeCleanup()
	}
	if s.cleanupErr != nil {
		return nil, s.cleanupErr
	}
	deleted := make([]RuntimeGroupStateRecord, 0, len(expected))
	for _, item := range expected {
		state, found := s.states[item.PluginKey]
		if !found {
			return nil, ErrRuntimeStateConflict
		}
		group, groupFound := findPersistedGroup(state.Groups, item.GroupID)
		if !groupFound || group.Version != item.Version {
			return nil, ErrRuntimeStateConflict
		}
		deleted = append(deleted, RuntimeGroupStateRecord{PluginKey: item.PluginKey, GroupID: item.GroupID, Enabled: group.Enabled, Version: group.Version, UpdatedAt: group.UpdatedAt})
		next := make([]PersistedGroupState, 0, len(state.Groups)-1)
		for _, current := range state.Groups {
			if current.GroupID != item.GroupID {
				next = append(next, current)
			}
		}
		state.Groups = next
		s.states[item.PluginKey] = state
	}
	return deleted, nil
}

func (s *runtimeServiceStore) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.events...)
}

// runtimeServiceLifecycle 把生命周期调用写入与仓库共享的顺序日志。
type runtimeServiceLifecycle struct {
	store      *runtimeServiceStore
	enableErr  error
	disableErr error
}

func (l *runtimeServiceLifecycle) OnEnable(context.Context) error {
	l.store.mu.Lock()
	l.store.events = append(l.store.events, "lifecycle-enable")
	l.store.mu.Unlock()
	return l.enableErr
}

func (l *runtimeServiceLifecycle) OnDisable(context.Context) error {
	l.store.mu.Lock()
	l.store.events = append(l.store.events, "lifecycle-disable")
	l.store.mu.Unlock()
	return l.disableErr
}

type runtimeServiceAuthorizer struct {
	err error
}

func (a *runtimeServiceAuthorizer) Authorize(management.Actor) error {
	return a.err
}

type runtimeServiceGroupDirectory struct {
	mu       sync.Mutex
	groups   []onebot.GroupInfo
	err      error
	noCache  []bool
	onLookup func()
}

func (d *runtimeServiceGroupDirectory) GetGroupList(_ context.Context, noCache bool) ([]onebot.GroupInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.onLookup != nil {
		d.onLookup()
	}
	d.noCache = append(d.noCache, noCache)
	return append([]onebot.GroupInfo(nil), d.groups...), d.err
}

func (d *runtimeServiceGroupDirectory) setGroups(groups ...onebot.GroupInfo) {
	d.mu.Lock()
	d.groups = append([]onebot.GroupInfo(nil), groups...)
	d.mu.Unlock()
}

func (d *runtimeServiceGroupDirectory) lookups() []bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]bool(nil), d.noCache...)
}

func newRuntimeServiceSubject(t *testing.T, store *runtimeServiceStore, authorizer *runtimeServiceAuthorizer, specs ...PluginSpec) (*RuntimeService, *RuntimeController) {
	t.Helper()
	catalog, err := NewSpecCatalog(specs)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewRuntimeService(catalog, controller, store, authorizer, &runtimeServiceGroupDirectory{})
	if err != nil {
		t.Fatal(err)
	}
	return service, controller
}

func newRuntimeGroupMaintenanceSubject(t *testing.T, store *runtimeServiceStore, groups *runtimeServiceGroupDirectory, specs ...PluginSpec) (*RuntimeService, *RuntimeController) {
	t.Helper()
	catalog, err := NewSpecCatalog(specs)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewRuntimeService(catalog, controller, store, &runtimeServiceAuthorizer{}, groups)
	if err != nil {
		t.Fatal(err)
	}
	return service, controller
}

func runtimeControllerGroupState(t *testing.T, controller *RuntimeController, pluginKey string, groupID int64) (bool, bool) {
	t.Helper()
	entry, found := controller.entry(pluginKey)
	if !found {
		t.Fatalf("controller plugin %s not found", pluginKey)
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	enabled, found := entry.groups[groupID]
	return enabled, found
}

func TestRuntimeServicePreviewsGroupCopyWithTargetDifference(t *testing.T) {
	zone := time.FixedZone("preview", 8*60*60)
	updatedAt := time.Date(2026, 7, 28, 12, 0, 0, 0, zone)
	store := newRuntimeServiceStore(
		PersistedPluginState{PluginKey: "echo", Groups: []PersistedGroupState{
			{GroupID: 100, Enabled: true, Version: 2, UpdatedAt: updatedAt},
			{GroupID: 200, Enabled: false, Version: 4, UpdatedAt: updatedAt},
		}},
		PersistedPluginState{PluginKey: "tools", Groups: []PersistedGroupState{{GroupID: 100, Enabled: false, Version: 3, UpdatedAt: updatedAt}}},
		PersistedPluginState{PluginKey: "legacy", Groups: []PersistedGroupState{{GroupID: 100, Enabled: true, Version: 9, UpdatedAt: updatedAt}}},
	)
	directory := &runtimeServiceGroupDirectory{groups: []onebot.GroupInfo{
		{GroupID: 200, GroupName: "target"},
		{GroupID: 100, GroupName: "source"},
	}}
	service, _ := newRuntimeGroupMaintenanceSubject(t, store, directory, validPluginSpec("tools", "tools"), validPluginSpec("echo", "echo"))

	preview, err := service.PreviewGroupCopy(context.Background(), runtimeServiceActor, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Source.GroupName != "source" || preview.Target.GroupName != "target" || len(preview.States) != 2 {
		t.Fatalf("preview = %+v", preview)
	}
	if preview.States[0].PluginKey != "echo" || !preview.States[0].Enabled || preview.States[0].SourceVersion != 2 || preview.States[0].TargetVersion != 4 || preview.States[0].TargetEnabled == nil || *preview.States[0].TargetEnabled {
		t.Fatalf("echo preview = %+v", preview.States[0])
	}
	if preview.States[1].PluginKey != "tools" || preview.States[1].TargetVersion != 0 || preview.States[1].TargetEnabled != nil {
		t.Fatalf("tools preview = %+v", preview.States[1])
	}
	if !reflect.DeepEqual(directory.lookups(), []bool{true}) {
		t.Fatalf("GetGroupList noCache = %v", directory.lookups())
	}
}

func TestRuntimeServiceCopiesGroupStatesWithMergeAndFailClosedOrdering(t *testing.T) {
	now := time.Now().UTC()
	store := newRuntimeServiceStore(
		PersistedPluginState{PluginKey: "echo", Groups: []PersistedGroupState{{GroupID: 100, Enabled: true, Version: 2, UpdatedAt: now}, {GroupID: 200, Enabled: false, Version: 4, UpdatedAt: now}}},
		PersistedPluginState{PluginKey: "tools", Groups: []PersistedGroupState{{GroupID: 100, Enabled: false, Version: 3, UpdatedAt: now}, {GroupID: 200, Enabled: true, Version: 5, UpdatedAt: now}}},
		PersistedPluginState{PluginKey: "extra", Groups: []PersistedGroupState{{GroupID: 200, Enabled: true, Version: 7, UpdatedAt: now}}},
	)
	directory := &runtimeServiceGroupDirectory{groups: []onebot.GroupInfo{{GroupID: 100}, {GroupID: 200}}}
	service, controller := newRuntimeGroupMaintenanceSubject(t, store, directory,
		validPluginSpec("tools", "tools"), validPluginSpec("extra", "extra"), validPluginSpec("echo", "echo"))
	for pluginKey, enabled := range map[string]bool{"echo": false, "tools": true, "extra": true} {
		if err := controller.SetGroupEnabled(pluginKey, 200, enabled); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := service.PreviewGroupCopy(context.Background(), runtimeServiceActor, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	store.beforeCopy = func() {
		if enabled, _ := runtimeControllerGroupState(t, controller, "echo", 200); enabled {
			t.Error("true target opened before transaction committed")
		}
		if enabled, _ := runtimeControllerGroupState(t, controller, "tools", 200); enabled {
			t.Error("false target was not pre-closed")
		}
		if enabled, _ := runtimeControllerGroupState(t, controller, "extra", 200); !enabled {
			t.Error("target-only state changed during merge copy")
		}
	}

	result, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 200, preview.States)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceGroupID != 100 || result.TargetGroupID != 200 || len(result.States) != 2 {
		t.Fatalf("result = %+v", result)
	}
	for pluginKey, want := range map[string]bool{"echo": true, "tools": false, "extra": true} {
		if got, found := runtimeControllerGroupState(t, controller, pluginKey, 200); !found || got != want {
			t.Fatalf("controller %s target = %t,%t want %t", pluginKey, got, found, want)
		}
	}
	if !reflect.DeepEqual(store.log(), []string{"persist-copy"}) {
		t.Fatalf("store log = %v", store.log())
	}
	if !reflect.DeepEqual(directory.lookups(), []bool{true, true}) {
		t.Fatalf("GetGroupList noCache = %v", directory.lookups())
	}
}

func TestRuntimeServiceCopyFailureRestoresTargetSnapshot(t *testing.T) {
	now := time.Now().UTC()
	failure := errors.New("copy failed")
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Groups: []PersistedGroupState{
		{GroupID: 100, Enabled: false, Version: 2, UpdatedAt: now},
		{GroupID: 200, Enabled: true, Version: 4, UpdatedAt: now},
	}})
	store.copyErr = failure
	directory := &runtimeServiceGroupDirectory{groups: []onebot.GroupInfo{{GroupID: 100}, {GroupID: 200}}}
	service, controller := newRuntimeGroupMaintenanceSubject(t, store, directory, validPluginSpec("echo", "echo"))
	if err := controller.SetGroupEnabled("echo", 200, true); err != nil {
		t.Fatal(err)
	}
	store.beforeCopy = func() {
		if enabled, _ := runtimeControllerGroupState(t, controller, "echo", 200); enabled {
			t.Error("false target was not pre-closed before persistence")
		}
	}
	preview, err := service.PreviewGroupCopy(context.Background(), runtimeServiceActor, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 200, preview.States); !errors.Is(err, failure) {
		t.Fatalf("CopyGroupStates() error = %v", err)
	}
	if enabled, found := runtimeControllerGroupState(t, controller, "echo", 200); !found || !enabled {
		t.Fatalf("restored copy target = %t,%t", enabled, found)
	}
}

func TestRuntimeServiceCopyFailureRestoresAbsentTargetSnapshot(t *testing.T) {
	now := time.Now().UTC()
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Groups: []PersistedGroupState{
		{GroupID: 100, Enabled: false, Version: 2, UpdatedAt: now},
	}})
	store.copyErr = errors.New("copy rolled back")
	directory := &runtimeServiceGroupDirectory{groups: []onebot.GroupInfo{{GroupID: 100}, {GroupID: 200}}}
	service, controller := newRuntimeGroupMaintenanceSubject(t, store, directory, validPluginSpec("echo", "echo"))
	preview, err := service.PreviewGroupCopy(context.Background(), runtimeServiceActor, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 200, preview.States); !errors.Is(err, store.copyErr) {
		t.Fatalf("CopyGroupStates() error = %v", err)
	}
	if enabled, found := runtimeControllerGroupState(t, controller, "echo", 200); found || enabled {
		t.Fatalf("restored absent target = %t,%t", enabled, found)
	}
}

func TestRuntimeServiceCopyUnknownCommitKeepsWholeBatchClosed(t *testing.T) {
	now := time.Now().UTC()
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Groups: []PersistedGroupState{
		{GroupID: 100, Enabled: false, Version: 2, UpdatedAt: now},
		{GroupID: 200, Enabled: true, Version: 4, UpdatedAt: now},
	}})
	store.copyErr = errors.Join(ErrRuntimeStateCommitUnknown, errors.New("connection lost"))
	directory := &runtimeServiceGroupDirectory{groups: []onebot.GroupInfo{{GroupID: 100}, {GroupID: 200}}}
	service, controller := newRuntimeGroupMaintenanceSubject(t, store, directory, validPluginSpec("echo", "echo"))
	if err := controller.SetGroupEnabled("echo", 200, true); err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewGroupCopy(context.Background(), runtimeServiceActor, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 200, preview.States); !errors.Is(err, ErrRuntimeStateCommitUnknown) {
		t.Fatalf("unknown commit error = %v", err)
	}
	if enabled, found := runtimeControllerGroupState(t, controller, "echo", 200); !found || enabled {
		t.Fatalf("unknown commit target = %t,%t", enabled, found)
	}
}

func TestRuntimeServiceCopyInvalidCommittedResultClosesWholeBatch(t *testing.T) {
	now := time.Now().UTC()
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Groups: []PersistedGroupState{
		{GroupID: 100, Enabled: true, Version: 2, UpdatedAt: now},
		{GroupID: 200, Enabled: false, Version: 4, UpdatedAt: now},
	}})
	directory := &runtimeServiceGroupDirectory{groups: []onebot.GroupInfo{{GroupID: 100}, {GroupID: 200}}}
	service, controller := newRuntimeGroupMaintenanceSubject(t, store, directory, validPluginSpec("echo", "echo"))
	if err := controller.SetGroupEnabled("echo", 200, false); err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewGroupCopy(context.Background(), runtimeServiceActor, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	store.copyResult = []RuntimeGroupStateRecord{{PluginKey: "echo", GroupID: 999, Enabled: true, Version: 5, UpdatedAt: now}}
	if _, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 200, preview.States); err == nil {
		t.Fatal("invalid committed result was accepted")
	}
	if enabled, found := runtimeControllerGroupState(t, controller, "echo", 200); !found || enabled {
		t.Fatalf("invalid committed result target = %t,%t", enabled, found)
	}
}

func TestRuntimeServiceCopyRejectsIncompletePreviewAndRechecksLiveGroups(t *testing.T) {
	now := time.Now().UTC()
	store := newRuntimeServiceStore(
		PersistedPluginState{PluginKey: "echo", Groups: []PersistedGroupState{{GroupID: 100, Enabled: true, Version: 2, UpdatedAt: now}}},
		PersistedPluginState{PluginKey: "tools", Groups: []PersistedGroupState{{GroupID: 100, Enabled: false, Version: 3, UpdatedAt: now}}},
	)
	directory := &runtimeServiceGroupDirectory{groups: []onebot.GroupInfo{{GroupID: 100}, {GroupID: 200}}}
	service, _ := newRuntimeGroupMaintenanceSubject(t, store, directory, validPluginSpec("echo", "echo"), validPluginSpec("tools", "tools"))
	preview, err := service.PreviewGroupCopy(context.Background(), runtimeServiceActor, 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 200, preview.States[:1]); !errors.Is(err, ErrRuntimeStateConflict) {
		t.Fatalf("incomplete copy error = %v", err)
	}
	if len(store.log()) != 0 {
		t.Fatalf("incomplete copy persisted: %v", store.log())
	}
	directory.setGroups(onebot.GroupInfo{GroupID: 100})
	if _, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 200, preview.States); !errors.Is(err, ErrRuntimeGroupNotLive) {
		t.Fatalf("departed target error = %v", err)
	}
	if len(store.log()) != 0 {
		t.Fatalf("departed target persisted: %v", store.log())
	}
}

func TestRuntimeServicePreviewsAndCleansStaleGroupStates(t *testing.T) {
	zone := time.FixedZone("stale", -5*60*60)
	updatedAt := time.Date(2026, 7, 28, 3, 0, 0, 0, zone)
	store := newRuntimeServiceStore(
		PersistedPluginState{PluginKey: "echo", Groups: []PersistedGroupState{{GroupID: 100, Enabled: true, Version: 1, UpdatedAt: updatedAt}, {GroupID: 200, Enabled: true, Version: 2, UpdatedAt: updatedAt}}},
		PersistedPluginState{PluginKey: "tools", Groups: []PersistedGroupState{{GroupID: 300, Enabled: false, Version: 3, UpdatedAt: updatedAt}}},
		PersistedPluginState{PluginKey: "legacy", Groups: []PersistedGroupState{{GroupID: 400, Enabled: true, Version: 4, UpdatedAt: updatedAt}}},
	)
	directory := &runtimeServiceGroupDirectory{groups: []onebot.GroupInfo{{GroupID: 100}}}
	service, controller := newRuntimeGroupMaintenanceSubject(t, store, directory, validPluginSpec("tools", "tools"), validPluginSpec("echo", "echo"))
	for _, state := range []RuntimeGroupStateRecord{{PluginKey: "echo", GroupID: 200, Enabled: true}, {PluginKey: "tools", GroupID: 300, Enabled: true}} {
		if err := controller.SetGroupEnabled(state.PluginKey, state.GroupID, state.Enabled); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := service.PreviewStaleGroupStates(context.Background(), runtimeServiceActor)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 3 || preview[0].PluginKey != "echo" || preview[0].GroupID != 200 || preview[1].PluginKey != "legacy" || preview[1].GroupID != 400 || preview[2].PluginKey != "tools" || preview[2].GroupID != 300 {
		t.Fatalf("stale preview = %+v", preview)
	}
	if preview[0].UpdatedAt.Location() != time.UTC || preview[1].UpdatedAt.Location() != time.UTC {
		t.Fatalf("stale preview timestamps = %+v", preview)
	}
	store.beforeCleanup = func() {
		for _, state := range preview {
			if state.PluginKey == "legacy" {
				continue
			}
			if enabled, _ := runtimeControllerGroupState(t, controller, state.PluginKey, state.GroupID); enabled {
				t.Errorf("%s:%d was not pre-closed", state.PluginKey, state.GroupID)
			}
		}
	}
	request := append([]RuntimeStaleGroupState(nil), preview...)
	request[0].Enabled = false
	request[0].UpdatedAt = time.Time{}
	deleted, err := service.CleanupStaleGroupStates(context.Background(), runtimeServiceActor, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 3 || !deleted[0].Enabled || deleted[0].UpdatedAt.Location() != time.UTC || deleted[1].PluginKey != "legacy" {
		t.Fatalf("deleted authority = %+v", deleted)
	}
	for _, state := range deleted {
		if state.PluginKey == "legacy" {
			continue
		}
		if enabled, found := runtimeControllerGroupState(t, controller, state.PluginKey, state.GroupID); found || enabled {
			t.Fatalf("deleted controller state %s:%d = %t,%t", state.PluginKey, state.GroupID, enabled, found)
		}
	}
	if !reflect.DeepEqual(directory.lookups(), []bool{true, true}) {
		t.Fatalf("GetGroupList noCache = %v", directory.lookups())
	}
}

func TestRuntimeServiceCleanupRechecksLiveAndFailureStaysClosed(t *testing.T) {
	now := time.Now().UTC()
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Groups: []PersistedGroupState{{GroupID: 200, Enabled: true, Version: 2, UpdatedAt: now}}})
	directory := &runtimeServiceGroupDirectory{}
	service, controller := newRuntimeGroupMaintenanceSubject(t, store, directory, validPluginSpec("echo", "echo"))
	if err := controller.SetGroupEnabled("echo", 200, true); err != nil {
		t.Fatal(err)
	}
	request := []RuntimeStaleGroupState{{PluginKey: "echo", GroupID: 200, Version: 2}}
	directory.setGroups(onebot.GroupInfo{GroupID: 200})
	if _, err := service.CleanupStaleGroupStates(context.Background(), runtimeServiceActor, request); !errors.Is(err, ErrRuntimeGroupStateNotStale) {
		t.Fatalf("live cleanup error = %v", err)
	}
	if enabled, _ := runtimeControllerGroupState(t, controller, "echo", 200); !enabled {
		t.Fatal("live validation changed controller")
	}
	store.cleanupErr = errors.New("cleanup failed")
	directory.setGroups()
	if _, err := service.CleanupStaleGroupStates(context.Background(), runtimeServiceActor, request); !errors.Is(err, store.cleanupErr) {
		t.Fatalf("failed cleanup error = %v", err)
	}
	if enabled, found := runtimeControllerGroupState(t, controller, "echo", 200); !found || enabled {
		t.Fatalf("failed cleanup controller = %t,%t", enabled, found)
	}
}

func TestRuntimeServiceGroupMaintenanceStableValidationErrors(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo"})
	directory := &runtimeServiceGroupDirectory{groups: []onebot.GroupInfo{{GroupID: 100}, {GroupID: 200}}}
	service, _ := newRuntimeGroupMaintenanceSubject(t, store, directory, validPluginSpec("echo", "echo"))
	if _, err := service.PreviewGroupCopy(context.Background(), runtimeServiceActor, 100, 200); !errors.Is(err, ErrRuntimeGroupSourceStateNotFound) {
		t.Fatalf("missing source state error = %v", err)
	}
	if _, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 100, []RuntimeGroupCopyItem{{PluginKey: "echo", SourceVersion: 1}}); !errors.Is(err, ErrRuntimeGroupCopySameGroup) {
		t.Fatalf("same group error = %v", err)
	}
	if _, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 200, nil); !errors.Is(err, ErrRuntimeGroupBatchEmpty) {
		t.Fatalf("empty copy error = %v", err)
	}
	if _, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 200, []RuntimeGroupCopyItem{{PluginKey: "missing", SourceVersion: 1}}); !errors.Is(err, ErrRuntimePluginNotFound) {
		t.Fatalf("unknown copy plugin error = %v", err)
	}
	tooManyCopies := make([]RuntimeGroupCopyItem, RuntimeGroupBatchLimit+1)
	if _, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 200, tooManyCopies); !errors.Is(err, ErrRuntimeGroupBatchTooLarge) {
		t.Fatalf("large copy error = %v", err)
	}
	cleanupTests := []struct {
		name   string
		states []RuntimeStaleGroupState
		want   error
	}{
		{name: "empty", want: ErrRuntimeGroupBatchEmpty},
		{name: "duplicate", states: []RuntimeStaleGroupState{{PluginKey: "echo", GroupID: 300, Version: 1}, {PluginKey: "echo", GroupID: 300, Version: 1}}, want: ErrRuntimeGroupStateDuplicate},
		{name: "unknown", states: []RuntimeStaleGroupState{{PluginKey: "missing", GroupID: 300, Version: 1}}, want: ErrRuntimeStateConflict},
		{name: "invalid version", states: []RuntimeStaleGroupState{{PluginKey: "echo", GroupID: 300}}, want: ErrRuntimeStateInvalidVersion},
		{name: "invalid group", states: []RuntimeStaleGroupState{{PluginKey: "echo", Version: 1}}, want: ErrInvalidRuntimeGroupID},
	}
	for _, test := range cleanupTests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.CleanupStaleGroupStates(context.Background(), runtimeServiceActor, test.states); !errors.Is(err, test.want) {
				t.Fatalf("CleanupStaleGroupStates() error = %v", err)
			}
		})
	}
	tooMany := make([]RuntimeStaleGroupState, RuntimeGroupBatchLimit+1)
	if _, err := service.CleanupStaleGroupStates(context.Background(), runtimeServiceActor, tooMany); !errors.Is(err, ErrRuntimeGroupBatchTooLarge) {
		t.Fatalf("large cleanup error = %v", err)
	}
}

func TestRuntimeServiceGroupDirectoryErrorsAreStable(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo"})
	directory := &runtimeServiceGroupDirectory{err: errors.New("onebot unavailable")}
	service, _ := newRuntimeGroupMaintenanceSubject(t, store, directory, validPluginSpec("echo", "echo"))
	if _, err := service.PreviewStaleGroupStates(context.Background(), runtimeServiceActor); !errors.Is(err, ErrRuntimeGroupDirectoryUnavailable) {
		t.Fatalf("directory failure error = %v", err)
	}
}

func TestNewRuntimeServiceRejectsMissingDependencies(t *testing.T) {
	catalog, err := NewSpecCatalog([]PluginSpec{validPluginSpec("echo", "echo")})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	var typedNilStore *runtimeServiceStore
	var typedNilGroups *runtimeServiceGroupDirectory
	tests := []struct {
		name       string
		catalog    *SpecCatalog
		controller *RuntimeController
		store      RuntimeStateStore
		authorizer RuntimeAuthorizer
		groups     RuntimeGroupDirectory
	}{
		{name: "catalog", controller: controller, store: newRuntimeServiceStore(), authorizer: &runtimeServiceAuthorizer{}, groups: &runtimeServiceGroupDirectory{}},
		{name: "controller", catalog: catalog, store: newRuntimeServiceStore(), authorizer: &runtimeServiceAuthorizer{}, groups: &runtimeServiceGroupDirectory{}},
		{name: "store", catalog: catalog, controller: controller, authorizer: &runtimeServiceAuthorizer{}, groups: &runtimeServiceGroupDirectory{}},
		{name: "typed nil store", catalog: catalog, controller: controller, store: typedNilStore, authorizer: &runtimeServiceAuthorizer{}, groups: &runtimeServiceGroupDirectory{}},
		{name: "authorizer", catalog: catalog, controller: controller, store: newRuntimeServiceStore(), groups: &runtimeServiceGroupDirectory{}},
		{name: "groups", catalog: catalog, controller: controller, store: newRuntimeServiceStore(), authorizer: &runtimeServiceAuthorizer{}},
		{name: "typed nil groups", catalog: catalog, controller: controller, store: newRuntimeServiceStore(), authorizer: &runtimeServiceAuthorizer{}, groups: typedNilGroups},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, err := NewRuntimeService(test.catalog, test.controller, test.store, test.authorizer, test.groups)
			if service != nil || err == nil {
				t.Fatalf("NewRuntimeService() = %v,%v", service, err)
			}
		})
	}
}

func TestRuntimeServiceReadsRequireAuthorization(t *testing.T) {
	forbidden := errors.New("forbidden")
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Version: 1})
	authorizer := &runtimeServiceAuthorizer{err: forbidden}
	service, _ := newRuntimeServiceSubject(t, store, authorizer, validPluginSpec("echo", "echo"))

	if _, err := service.List(context.Background(), runtimeServiceActor); !errors.Is(err, forbidden) {
		t.Fatalf("List() error = %v", err)
	}
	if _, err := service.Get(context.Background(), runtimeServiceActor, "echo"); !errors.Is(err, forbidden) {
		t.Fatalf("Get() error = %v", err)
	}
	if _, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", true, 1); !errors.Is(err, forbidden) {
		t.Fatalf("SetGlobalEnabled() error = %v", err)
	}
	if _, err := service.SetGroupEnabled(context.Background(), runtimeServiceActor, "echo", 100, true, 0); !errors.Is(err, forbidden) {
		t.Fatalf("SetGroupEnabled() error = %v", err)
	}
	if _, err := service.PreviewGroupCopy(context.Background(), runtimeServiceActor, 100, 200); !errors.Is(err, forbidden) {
		t.Fatalf("PreviewGroupCopy() error = %v", err)
	}
	if _, err := service.CopyGroupStates(context.Background(), runtimeServiceActor, 100, 200, []RuntimeGroupCopyItem{{PluginKey: "echo", SourceVersion: 1}}); !errors.Is(err, forbidden) {
		t.Fatalf("CopyGroupStates() error = %v", err)
	}
	if _, err := service.PreviewStaleGroupStates(context.Background(), runtimeServiceActor); !errors.Is(err, forbidden) {
		t.Fatalf("PreviewStaleGroupStates() error = %v", err)
	}
	if _, err := service.CleanupStaleGroupStates(context.Background(), runtimeServiceActor, []RuntimeStaleGroupState{{PluginKey: "echo", GroupID: 100, Version: 1}}); !errors.Is(err, forbidden) {
		t.Fatalf("CleanupStaleGroupStates() error = %v", err)
	}
	if len(store.log()) != 0 {
		t.Fatalf("unauthorized calls touched store: %v", store.log())
	}
}

func TestRuntimeServiceListExposesIntentAndRuntimeStatus(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{
		PluginKey: "echo", DesiredEnabled: true, Version: 2, UpdatedAt: time.Now(),
		Groups: []PersistedGroupState{{GroupID: 100, Enabled: true, Version: 1, UpdatedAt: time.Now()}},
	})
	lifecycle := &runtimeServiceLifecycle{store: store, enableErr: errors.New("boom")}
	echo := validPluginSpec("echo", "echo")
	echo.Lifecycle = lifecycle
	service, controller := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, echo, validPluginSpec("tools", "tools"))
	if err := controller.Enable(context.Background(), "echo"); err == nil {
		t.Fatal("expected lifecycle failure")
	}

	views, err := service.List(context.Background(), runtimeServiceActor)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].PluginKey != "echo" || views[1].PluginKey != "tools" {
		t.Fatalf("views = %+v", views)
	}
	if !views[0].DesiredEnabled || views[0].Status != RuntimeFailed || views[0].LastError == "" {
		t.Fatalf("echo view = %+v", views[0])
	}
	if views[0].UpdatedAt.Location() != time.UTC || len(views[0].Groups) != 1 || views[0].Groups[0].GroupID != 100 {
		t.Fatalf("echo groups = %+v", views[0])
	}
	// 目录中存在但尚无状态行的插件必须按默认关闭展示。
	if views[1].DesiredEnabled || views[1].Status != RuntimeDisabled || views[1].Version != 0 {
		t.Fatalf("tools view = %+v", views[1])
	}
	// 代码持有的命令声明必须只读暴露，且身份集合顺序稳定。
	command := views[0].Commands[0]
	if len(views[0].Commands) != 1 || command.Key != "run" || command.Scope != string(CommandScopeGroup) {
		t.Fatalf("commands = %+v", views[0].Commands)
	}
	if !equalStrings(command.AllowedRoles, []string{"group_member"}) || !equalStrings(command.Triggers, []string{"echo"}) {
		t.Fatalf("command roles=%v triggers=%v", command.AllowedRoles, command.Triggers)
	}
}

func TestRuntimeServiceGetReturnsViewAndPropagatesStoreErrors(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{
		PluginKey: "echo", DesiredEnabled: true, Version: 2,
		Groups: []PersistedGroupState{{GroupID: 100, Enabled: true, Version: 1}},
	})
	service, _ := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, validPluginSpec("echo", "echo"))

	view, err := service.Get(context.Background(), runtimeServiceActor, "echo")
	if err != nil {
		t.Fatal(err)
	}
	if view.PluginKey != "echo" || view.DisplayName == "" || !view.DesiredEnabled || view.Version != 2 || len(view.Groups) != 1 {
		t.Fatalf("view = %+v", view)
	}

	storeFailure := errors.New("store failed")
	store.findErr, store.loadErr = storeFailure, storeFailure
	if _, err := service.Get(context.Background(), runtimeServiceActor, "echo"); !errors.Is(err, storeFailure) {
		t.Fatalf("Get() error = %v", err)
	}
	if _, err := service.List(context.Background(), runtimeServiceActor); !errors.Is(err, storeFailure) {
		t.Fatalf("List() error = %v", err)
	}
	if _, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", false, 2); !errors.Is(err, storeFailure) {
		t.Fatalf("SetGlobalEnabled() error = %v", err)
	}
	if _, err := service.SetGroupEnabled(context.Background(), runtimeServiceActor, "echo", 100, false, 1); !errors.Is(err, storeFailure) {
		t.Fatalf("SetGroupEnabled() error = %v", err)
	}
}

func TestRuntimeServiceGetRejectsUnknownPluginBeforeStore(t *testing.T) {
	store := newRuntimeServiceStore()
	service, _ := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, validPluginSpec("echo", "echo"))
	if _, err := service.Get(context.Background(), runtimeServiceActor, "missing"); !errors.Is(err, ErrRuntimePluginNotFound) {
		t.Fatalf("Get(missing) error = %v", err)
	}
	if _, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "missing", true, 1); !errors.Is(err, ErrRuntimePluginNotFound) {
		t.Fatalf("SetGlobalEnabled(missing) error = %v", err)
	}
	if len(store.log()) != 0 {
		t.Fatalf("unknown plugin touched store: %v", store.log())
	}
}

func TestRuntimeServiceEnablePersistsIntentBeforeLifecycle(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Version: 1})
	echo := validPluginSpec("echo", "echo")
	echo.Lifecycle = &runtimeServiceLifecycle{store: store}
	service, controller := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, echo)

	view, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"persist", "lifecycle-enable"}; !equalStrings(store.log(), want) {
		t.Fatalf("order = %v", store.log())
	}
	if !view.DesiredEnabled || view.Version != 2 || view.Status != RuntimeReady {
		t.Fatalf("view = %+v", view)
	}
	if state, _ := controller.State("echo"); state.Status != RuntimeReady {
		t.Fatalf("controller status = %v", state.Status)
	}
	if len(store.actors) != 1 || store.actors[0].ID != "10001" || store.actors[0].RequestID != "req-1" {
		t.Fatalf("actor not forwarded for audit: %+v", store.actors)
	}
}

func TestRuntimeServiceEnableKeepsIntentWhenLifecycleFails(t *testing.T) {
	lifecycleFailure := errors.New("enable failed")
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Version: 1})
	echo := validPluginSpec("echo", "echo")
	echo.Lifecycle = &runtimeServiceLifecycle{store: store, enableErr: lifecycleFailure}
	service, controller := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, echo)

	view, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", true, 1)
	if !errors.Is(err, lifecycleFailure) {
		t.Fatalf("SetGlobalEnabled() error = %v", err)
	}
	// 意图保留，运行时停在 failed 并拒绝流量，管理员可看到分歧。
	if !view.DesiredEnabled || view.Status != RuntimeFailed || view.LastError == "" {
		t.Fatalf("view = %+v", view)
	}
	if _, admitted := controller.Admit("echo"); admitted {
		t.Fatal("failed plugin admitted traffic")
	}
	// 故障插件必须先关闭清理才能重新启用。
	if _, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", false, 2); err != nil {
		t.Fatal(err)
	}
	if state, _ := controller.State("echo"); state.Status != RuntimeDisabled {
		t.Fatalf("controller status = %v", state.Status)
	}
}

func TestRuntimeServiceDisableStopsRuntimeBeforePersist(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", DesiredEnabled: true, Version: 3})
	echo := validPluginSpec("echo", "echo")
	echo.Lifecycle = &runtimeServiceLifecycle{store: store}
	service, controller := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, echo)
	if err := controller.Enable(context.Background(), "echo"); err != nil {
		t.Fatal(err)
	}
	store.events = nil

	view, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", false, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lifecycle-disable", "persist"}; !equalStrings(store.log(), want) {
		t.Fatalf("order = %v", store.log())
	}
	if view.DesiredEnabled || view.Version != 4 || view.Status != RuntimeDisabled {
		t.Fatalf("view = %+v", view)
	}
}

func TestRuntimeServiceDisablePersistFailureLeavesRuntimeStopped(t *testing.T) {
	persistFailure := errors.New("persist failed")
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", DesiredEnabled: true, Version: 3})
	echo := validPluginSpec("echo", "echo")
	echo.Lifecycle = &runtimeServiceLifecycle{store: store}
	service, controller := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, echo)
	if err := controller.Enable(context.Background(), "echo"); err != nil {
		t.Fatal(err)
	}
	store.updateErr = persistFailure

	if _, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", false, 3); !errors.Is(err, persistFailure) {
		t.Fatalf("SetGlobalEnabled() error = %v", err)
	}
	// 落库失败停在安全侧：运行时已经不再接收流量。
	if _, admitted := controller.Admit("echo"); admitted {
		t.Fatal("runtime still admitted traffic after failed persist")
	}
}

func TestRuntimeServiceDisablePersistsIntentWhenCleanupFails(t *testing.T) {
	cleanupFailure := errors.New("disable failed")
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", DesiredEnabled: true, Version: 3})
	echo := validPluginSpec("echo", "echo")
	echo.Lifecycle = &runtimeServiceLifecycle{store: store, disableErr: cleanupFailure}
	service, controller := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, echo)
	if err := controller.Enable(context.Background(), "echo"); err != nil {
		t.Fatal(err)
	}

	view, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", false, 3)
	if !errors.Is(err, cleanupFailure) {
		t.Fatalf("SetGlobalEnabled() error = %v", err)
	}
	// 清理失败仍需落库关闭意图，避免重启后复活管理员已关停的插件。
	if view.DesiredEnabled || view.Status != RuntimeFailed {
		t.Fatalf("view = %+v", view)
	}
	if state, _ := store.FindState(context.Background(), "echo"); state.DesiredEnabled {
		t.Fatalf("persisted state = %+v", state)
	}
	if _, admitted := controller.Admit("echo"); admitted {
		t.Fatal("failed cleanup still admitted traffic")
	}
}

func TestRuntimeServiceGlobalWriteRejectsStaleVersionAndIsIdempotent(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", DesiredEnabled: true, Version: 3})
	echo := validPluginSpec("echo", "echo")
	echo.Lifecycle = &runtimeServiceLifecycle{store: store}
	service, _ := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, echo)

	if _, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", false, 2); !errors.Is(err, ErrRuntimeStateConflict) {
		t.Fatalf("stale version error = %v", err)
	}
	view, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", true, 3)
	if err != nil {
		t.Fatal(err)
	}
	// 已是目标意图时不写库、不审计、不驱动生命周期。
	if view.Version != 3 || len(store.log()) != 0 {
		t.Fatalf("idempotent call wrote: view=%+v events=%v", view, store.log())
	}
}

func TestRuntimeServiceGroupSwitchOrdersWritesAndKeepsIntentOffline(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Version: 1})
	service, controller := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, validPluginSpec("echo", "echo"))

	// 全局关闭时仍允许维护群意图。
	view, err := service.SetGroupEnabled(context.Background(), runtimeServiceActor, "echo", 100, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Groups) != 1 || view.Groups[0].GroupID != 100 || !view.Groups[0].Enabled || view.Groups[0].Version != 1 {
		t.Fatalf("view groups = %+v", view.Groups)
	}
	if view.Status != RuntimeDisabled {
		t.Fatalf("group switch changed runtime status: %+v", view)
	}
	if len(store.actors) != 1 || store.actors[0].ID != "10001" {
		t.Fatalf("actor not forwarded for audit: %+v", store.actors)
	}

	if err := controller.Enable(context.Background(), "echo"); err != nil {
		t.Fatal(err)
	}
	admission, admitted := controller.Admit("echo")
	if !admitted || !admission.GroupEnabled(100) || admission.GroupEnabled(200) {
		t.Fatal("group gate not applied to runtime")
	}
	admission.Release()

	if _, err := service.SetGroupEnabled(context.Background(), runtimeServiceActor, "echo", 100, false, 1); err != nil {
		t.Fatal(err)
	}
	admission, admitted = controller.Admit("echo")
	if !admitted || admission.GroupEnabled(100) {
		t.Fatal("group gate not closed after disable")
	}
	admission.Release()
}

func TestRuntimeServiceGroupWriteValidatesVersionAndGroupID(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{
		PluginKey: "echo", Version: 1,
		Groups: []PersistedGroupState{{GroupID: 100, Enabled: true, Version: 2}},
	})
	service, _ := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, validPluginSpec("echo", "echo"))

	tests := []struct {
		name            string
		groupID         int64
		enabled         bool
		expectedVersion int64
		want            error
	}{
		{name: "invalid group", groupID: 0, enabled: true, want: ErrInvalidRuntimeGroupID},
		{name: "stale version", groupID: 100, enabled: false, expectedVersion: 1, want: ErrRuntimeStateConflict},
		{name: "insert over existing", groupID: 100, enabled: false, expectedVersion: 0, want: ErrRuntimeStateConflict},
		{name: "update missing record", groupID: 200, enabled: true, expectedVersion: 1, want: ErrRuntimeStateConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.SetGroupEnabled(context.Background(), runtimeServiceActor, "echo", test.groupID, test.enabled, test.expectedVersion); !errors.Is(err, test.want) {
				t.Fatalf("SetGroupEnabled() error = %v", err)
			}
		})
	}
	// 已是目标值时不产生写入。
	if _, err := service.SetGroupEnabled(context.Background(), runtimeServiceActor, "echo", 100, true, 2); err != nil {
		t.Fatal(err)
	}
	if len(store.log()) != 0 {
		t.Fatalf("rejected or idempotent calls wrote: %v", store.log())
	}
}

func TestRuntimeServiceRejectsTransitionAndRecoveryBeforePersist(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Version: 1})
	lifecycle := &runtimeTestLifecycle{enableStarted: make(chan struct{}), enableBlock: make(chan struct{})}
	echo := validPluginSpec("echo", "echo")
	echo.Lifecycle = lifecycle
	service, controller := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, echo)

	go func() { _ = controller.Enable(context.Background(), "echo") }()
	<-lifecycle.enableStarted
	if _, err := service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", true, 1); !errors.Is(err, ErrRuntimeTransition) {
		t.Fatalf("transition error = %v", err)
	}
	close(lifecycle.enableBlock)
	eventually(t, func() bool {
		state, _ := controller.State("echo")
		return state.Status == RuntimeReady
	})

	failing := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Version: 1})
	failingSpec := validPluginSpec("echo", "echo")
	failingSpec.Lifecycle = &runtimeServiceLifecycle{store: failing, enableErr: errors.New("boom")}
	failingService, failingController := newRuntimeServiceSubject(t, failing, &runtimeServiceAuthorizer{}, failingSpec)
	if err := failingController.Enable(context.Background(), "echo"); err == nil {
		t.Fatal("expected lifecycle failure")
	}
	failing.events = nil
	if _, err := failingService.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", true, 1); !errors.Is(err, ErrRuntimeRecoveryNeeded) {
		t.Fatalf("recovery error = %v", err)
	}
	if len(store.log()) != 0 || len(failing.log()) != 0 {
		t.Fatal("rejected transition persisted intent")
	}
}

func TestRuntimeServiceSerializesConcurrentWritesPerPlugin(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Version: 1})
	service, _ := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, validPluginSpec("echo", "echo"))

	var group sync.WaitGroup
	results := make([]error, 8)
	for index := range results {
		group.Add(1)
		go func(slot int) {
			defer group.Done()
			_, results[slot] = service.SetGlobalEnabled(context.Background(), runtimeServiceActor, "echo", true, 1)
		}(index)
	}
	group.Wait()

	succeeded := 0
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrRuntimeStateConflict):
		default:
			t.Fatalf("unexpected error = %v", err)
		}
	}
	// 乐观锁下同一版本只允许一次成功切换，其余必须是冲突。
	if succeeded != 1 {
		t.Fatalf("succeeded = %d", succeeded)
	}
}

func equalStrings(actual, want []string) bool {
	if len(actual) != len(want) {
		return false
	}
	for index := range want {
		if actual[index] != want[index] {
			return false
		}
	}
	return true
}

// runtimeConfigProbe 记录配置钩子的调用顺序与最终发布的快照。
type runtimeConfigProbe struct {
	mu          sync.Mutex
	applied     []string
	validated   []string
	applyErr    error
	applyPanic  bool
	validateErr error
}

func (p *runtimeConfigProbe) validate(_ context.Context, raw json.RawMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.validated = append(p.validated, string(raw))
	return p.validateErr
}

func (p *runtimeConfigProbe) apply(_ context.Context, raw json.RawMessage) error {
	p.mu.Lock()
	shouldPanic, err := p.applyPanic, p.applyErr
	p.applied = append(p.applied, string(raw))
	p.mu.Unlock()
	if shouldPanic {
		panic("apply panic")
	}
	return err
}

func (p *runtimeConfigProbe) snapshots() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.applied...)
}

func runtimeConfigSpec(probe *runtimeConfigProbe) PluginSpec {
	spec := validPluginSpec("echo", "echo")
	spec.Config = &ConfigSpec{
		Schema: ConfigSchema{Fields: []ConfigField{
			{Key: "response_prefix", DisplayName: "回复前缀", Type: FieldString, Default: json.RawMessage(`""`)},
			{Key: "token", DisplayName: "令牌", Type: FieldSecret},
		}},
		Validate: probe.validate,
		Apply:    probe.apply,
	}
	return spec
}

func newRuntimeConfigSubject(t *testing.T, probe *runtimeConfigProbe, stored string) (*RuntimeService, *runtimeServiceStore) {
	t.Helper()
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Version: 1})
	store.configs["echo"] = PersistedPluginConfig{PluginKey: "echo", ConfigJSON: json.RawMessage(stored), Version: 1}
	service, _ := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, runtimeConfigSpec(probe))
	return service, store
}

func TestRuntimeServiceGetConfigRedactsSecretsAndFillsDefaults(t *testing.T) {
	probe := &runtimeConfigProbe{}
	service, _ := newRuntimeConfigSubject(t, probe, `{"token":"s3cret"}`)
	view, err := service.GetConfig(context.Background(), runtimeServiceActor, "echo")
	if err != nil {
		t.Fatal(err)
	}
	// secret 不得出现在管理读取结果中，缺失字段由 Schema 默认值补齐。
	if strings.Contains(string(view.Config), "s3cret") {
		t.Fatalf("secret leaked: %s", view.Config)
	}
	if !strings.Contains(string(view.Config), `"response_prefix":""`) {
		t.Fatalf("default missing: %s", view.Config)
	}
	if view.Version != 1 || len(view.Schema.Fields) != 2 || view.PluginKey != "echo" {
		t.Fatalf("view = %+v", view)
	}
}

func TestRuntimeServiceConfigRejectsUnsupportedPlugin(t *testing.T) {
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Version: 1})
	service, _ := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, validPluginSpec("echo", "echo"))
	if _, err := service.GetConfig(context.Background(), runtimeServiceActor, "echo"); !errors.Is(err, ErrRuntimeConfigNotSupported) {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if _, err := service.SetConfig(context.Background(), runtimeServiceActor, "echo", json.RawMessage(`{}`), 1); !errors.Is(err, ErrRuntimeConfigNotSupported) {
		t.Fatalf("SetConfig() error = %v", err)
	}
	// 未声明配置的插件不应触达存储层。
	if len(store.log()) != 0 {
		t.Fatalf("store called: %v", store.log())
	}
}

func TestRuntimeServiceSetConfigPersistsThenApplies(t *testing.T) {
	probe := &runtimeConfigProbe{}
	service, store := newRuntimeConfigSubject(t, probe, `{"response_prefix":"","token":"s3cret"}`)
	view, err := service.SetConfig(context.Background(), runtimeServiceActor, "echo", json.RawMessage(`{"response_prefix":"[bot] "}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(store.log(), []string{"persist-config"}) {
		t.Fatalf("events = %v", store.log())
	}
	applied := probe.snapshots()
	// 未提交的 secret 必须保留原值，而不是被合并成空字符串。
	if len(applied) != 1 || !strings.Contains(applied[0], `"token":"s3cret"`) || !strings.Contains(applied[0], `"response_prefix":"[bot] "`) {
		t.Fatalf("applied = %v", applied)
	}
	if view.Version != 2 || strings.Contains(string(view.Config), "s3cret") {
		t.Fatalf("view = %+v", view)
	}
	if len(store.actors) != 1 || store.actors[0].RequestID != "req-1" {
		t.Fatalf("actor not forwarded for audit: %+v", store.actors)
	}
}

func TestRuntimeServiceSetConfigRejectsStaleVersionAndInvalidInput(t *testing.T) {
	probe := &runtimeConfigProbe{}
	service, store := newRuntimeConfigSubject(t, probe, `{"response_prefix":""}`)
	if _, err := service.SetConfig(context.Background(), runtimeServiceActor, "echo", json.RawMessage(`{}`), 5); !errors.Is(err, ErrRuntimeStateConflict) {
		t.Fatalf("stale version error = %v", err)
	}
	// 未知字段必须在写库前被规范化拒绝。
	if _, err := service.SetConfig(context.Background(), runtimeServiceActor, "echo", json.RawMessage(`{"unknown":1}`), 1); !errors.Is(err, ErrRuntimeConfigInvalid) {
		t.Fatalf("unknown field error = %v", err)
	}
	probe.validateErr = errors.New("domain rejected")
	if _, err := service.SetConfig(context.Background(), runtimeServiceActor, "echo", json.RawMessage(`{"response_prefix":"x"}`), 1); !errors.Is(err, ErrRuntimeConfigInvalid) {
		t.Fatalf("domain validation error = %v", err)
	}
	if len(store.log()) != 0 || len(probe.snapshots()) != 0 {
		t.Fatalf("rejected input reached persistence: events=%v applied=%v", store.log(), probe.snapshots())
	}
}

func TestRuntimeServiceSetConfigCompensatesWhenApplyFails(t *testing.T) {
	tests := []struct {
		name       string
		applyErr   error
		applyPanic bool
	}{
		{name: "返回错误", applyErr: errors.New("apply failed")},
		{name: "发生 panic", applyPanic: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe := &runtimeConfigProbe{applyErr: test.applyErr, applyPanic: test.applyPanic}
			service, store := newRuntimeConfigSubject(t, probe, `{"response_prefix":"旧"}`)
			_, err := service.SetConfig(context.Background(), runtimeServiceActor, "echo", json.RawMessage(`{"response_prefix":"新"}`), 1)
			if err == nil {
				t.Fatal("expected apply failure")
			}
			// 热应用失败后数据库必须补偿回旧值，避免持久化配置与运行快照长期不一致。
			restored := store.configs["echo"]
			if !strings.Contains(string(restored.ConfigJSON), "旧") {
				t.Fatalf("config not compensated: %s", restored.ConfigJSON)
			}
			if !equalStrings(store.log(), []string{"persist-config", "persist-config"}) {
				t.Fatalf("events = %v", store.log())
			}
		})
	}
}

func TestRuntimeServiceStateViewMarksConfigurablePlugins(t *testing.T) {
	probe := &runtimeConfigProbe{}
	store := newRuntimeServiceStore(PersistedPluginState{PluginKey: "echo", Version: 1}, PersistedPluginState{PluginKey: "tools", Version: 1})
	service, _ := newRuntimeServiceSubject(t, store, &runtimeServiceAuthorizer{}, runtimeConfigSpec(probe), validPluginSpec("tools", "tools"))
	views, err := service.List(context.Background(), runtimeServiceActor)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || !views[0].HasConfig || views[1].HasConfig {
		t.Fatalf("has_config = %v,%v", views[0].HasConfig, views[1].HasConfig)
	}
}
