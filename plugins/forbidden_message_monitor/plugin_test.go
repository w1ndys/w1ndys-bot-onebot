// 📌 影响范围：纯内存验证群内违禁消息监控插件的规格、依赖、生命周期与空处理行为；不访问外部服务。
package forbiddenmessagemonitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/w1ndys/w1ndys-bot/internal/management"
	"github.com/w1ndys/w1ndys-bot/internal/onebot"
	"github.com/w1ndys/w1ndys-bot/internal/plugin"
	"github.com/w1ndys/w1ndys-bot/internal/ws"
)

type fakeActions struct {
	banParams     []onebot.SetGroupBanParams
	history       onebot.GetGroupMessageHistoryResult
	historyParams []onebot.GetGroupMessageHistoryParams
	messageInfo   onebot.MessageInfo
	deleted       []any
	err           error
}

type blockingMaintenanceRepository struct {
	*fakeMonitorRepository
	started chan struct{}
	release chan struct{}
}

type contextBlockingMaintenanceRepository struct {
	*fakeMonitorRepository
	started chan struct{}
}

type maintenanceContextKey struct{}

func (f *contextBlockingMaintenanceRepository) ListObservedGroups(ctx context.Context) ([]int64, error) {
	if ctx.Value(maintenanceContextKey{}) == true {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type maintenanceActions struct {
	*fakeActions
	memberListCalls  []string
	memberListErrors map[string]error
	waitForCancel    map[string]bool
	members          map[string][]onebot.GroupMemberInfo
}

func (f *maintenanceActions) GetGroupMemberList(ctx context.Context, params onebot.GetGroupMemberListParams) ([]onebot.GroupMemberInfo, error) {
	f.memberListCalls = append(f.memberListCalls, params.GroupID)
	if f.waitForCancel[params.GroupID] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.members[params.GroupID], f.memberListErrors[params.GroupID]
}

type maintenanceRepository struct {
	*fakeMonitorRepository
	groups           []int64
	listCalls        int
	recentCalls      []int64
	recentErrors     map[int64]error
	replaceCalls     []int64
	replacementError map[int64]error
}

func (f *maintenanceRepository) ListObservedGroups(context.Context) ([]int64, error) {
	f.listCalls++
	return f.groups, nil
}

func (f *maintenanceRepository) RecentValidCounts(_ context.Context, groupID int64, _ time.Time) (map[int64]int, error) {
	f.recentCalls = append(f.recentCalls, groupID)
	return map[int64]int{}, f.recentErrors[groupID]
}

func (f *maintenanceRepository) ReplaceWhitelist(_ context.Context, groupID int64, _ []int64, _ time.Time) error {
	f.replaceCalls = append(f.replaceCalls, groupID)
	return f.replacementError[groupID]
}

type blockingEnableLifecycle struct {
	started chan struct{}
	release chan struct{}
}

func (l *blockingEnableLifecycle) OnEnable(context.Context) error {
	close(l.started)
	<-l.release
	return nil
}

func (*blockingEnableLifecycle) OnDisable(context.Context) error { return nil }

type fakeLLMEvaluator struct {
	calls    int
	requests []LLMEvaluationRequest
	result   LLMEvaluationResult
	err      error
}

// Evaluate 返回预设模型结论并记录调用次数。
// @param ctx/request：模型调用参数。
// @returns 预设结果与错误。
// ⚠️副作用说明：递增calls。
func (f *fakeLLMEvaluator) Evaluate(_ context.Context, request LLMEvaluationRequest) (LLMEvaluationResult, error) {
	f.calls++
	f.requests = append(f.requests, request)

	// >>> 数据演变示例
	// 1. Low/pass -> calls1 -> 返回安全结论。
	// 2. 预设error -> calls+1 -> 返回error。
	return f.result, f.err
}

// ListObservedGroups 模拟不及时响应取消的外部维护调用。
// @param ctx：维护上下文。
// @returns release关闭后返回空群集合。
// ⚠️副作用说明：关闭started并阻塞到release关闭。
func (f *blockingMaintenanceRepository) ListObservedGroups(context.Context) ([]int64, error) {
	select {
	case <-f.started:
	default:
		close(f.started)
	}
	<-f.release

	// >>> 数据演变示例
	// 1. 首次维护 -> started关闭 -> release前阻塞。
	// 2. release关闭 -> 返回空群集合并允许worker退出。
	return []int64{}, nil
}

// SetGroupBan 记录测试禁言参数。
// @param ctx/params：Action参数。
// @returns 预设错误。
// ⚠️副作用说明：追加banParams。
func (f *fakeActions) SetGroupBan(_ context.Context, params onebot.SetGroupBanParams) error {
	f.banParams = append(f.banParams, params)

	// >>> 数据演变示例
	// 1. duration2592000 -> 记录 -> nil。
	// 2. 预设error -> 记录 -> error。
	return f.err
}

// GetGroupMemberList 返回空测试成员列表。
// @param ctx/params：Action参数。
// @returns 空列表与预设错误。
// ⚠️副作用说明：无。
func (f *fakeActions) GetGroupMemberList(context.Context, onebot.GetGroupMemberListParams) ([]onebot.GroupMemberInfo, error) {
	// >>> 数据演变示例
	// 1. group100 -> []。
	// 2. 预设error -> [],error。
	return []onebot.GroupMemberInfo{}, f.err
}

// GetGroupMessageHistory 返回预设测试历史。
// @param ctx/params：Action参数。
// @returns 预设历史与错误。
// ⚠️副作用说明：无。
func (f *fakeActions) GetGroupMessageHistory(_ context.Context, params onebot.GetGroupMessageHistoryParams) (onebot.GetGroupMessageHistoryResult, error) {
	f.historyParams = append(f.historyParams, params)
	// >>> 数据演变示例
	// 1. history含2条 -> 原样返回。
	// 2. 预设error -> history,error。
	return f.history, f.err
}

// GetMessage 返回用于补齐历史锚点的消息详情。
// @param ctx/messageID：查询参数。
// @returns 预设消息详情和错误。
// ⚠️副作用说明：无。
func (f *fakeActions) GetMessage(context.Context, any) (onebot.MessageInfo, error) {
	// >>> 数据演变示例
	// 1. messageInfo序号222 -> 原样返回。
	// 2. 预设错误 -> messageInfo,error。
	return f.messageInfo, f.err
}

// DeleteMessage 记录测试撤回ID。
// @param ctx/messageID：Action参数。
// @returns 预设错误。
// ⚠️副作用说明：追加deleted。
func (f *fakeActions) DeleteMessage(_ context.Context, messageID any) error {
	f.deleted = append(f.deleted, messageID)

	// >>> 数据演变示例
	// 1. id9 -> deleted=[9]。
	// 2. 预设error -> 仍记录并返回error。
	return f.err
}

// testImplementation 创建具备默认引擎与fake依赖的测试实例。
// @param 无。
// @returns 可执行消息与生命周期的实例。
// ⚠️副作用说明：仅分配内存。
func testImplementation() *implementation {
	engineConfig := DefaultEngineConfig()
	engine, _ := NewEngine(engineConfig)
	result := &implementation{actions: &fakeActions{}, repository: &fakeMonitorRepository{}, lexiconStore: staticLexicon{}, httpClient: &http.Client{}, maintenanceTimeout: maintenanceRunTimeout, now: func() time.Time { return time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC) }}
	result.snapshot.Store(&runtimeSnapshot{engine: engine, engineConfig: engineConfig, llmTimeout: time.Second, llmMaxConcurrency: 2, llmDailyRequestLimit: 500, minLLMMessageLength: 30})

	// >>> 数据演变示例
	// 1. 默认配置 -> Engine -> 可执行实例。
	// 2. fake仓储/Action -> 无外部访问。
	return result
}

// TestSpecContract 验证目标架构下的稳定身份、观察器与小型配置边界。
// @param t：Go测试上下文。
// @returns 无。
// ⚠️副作用说明：仅构造内存实例。
func TestSpecContract(t *testing.T) {
	service := &Service{implementation: testImplementation(), authorizer: staticAuthorizer{}}
	spec := service.Spec()
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if spec.Key != "forbidden_message_monitor" || spec.AdminPageKey != "forbidden_message_monitor" {
		t.Fatalf("spec = %+v", spec)
	}
	// 三类事件都由同一个观察器承担，且必须声明群消息与群通知。
	if len(spec.Commands) != 0 || len(spec.Observers) != 1 || len(spec.Observers[0].EventKinds) != 2 {
		t.Fatalf("observers = %+v", spec.Observers)
	}
	if spec.Observers[0].Phase != plugin.ObserverPhaseBeforeCommands {
		t.Fatalf("observer phase = %q", spec.Observers[0].Phase)
	}
	// 阈值与模型参数属于小型标量配置，词库不再出现在 Schema 中。
	if spec.Config == nil || spec.Lifecycle == nil {
		t.Fatalf("config/lifecycle missing: %+v", spec)
	}
	configKeys := make(map[string]struct{}, len(spec.Config.Schema.Fields))
	for _, field := range spec.Config.Schema.Fields {
		configKeys[field.Key] = struct{}{}
		if strings.HasSuffix(field.Key, "_json") {
			t.Fatalf("结构化字段仍在小型配置中: %s", field.Key)
		}
	}
	for _, required := range []string{"violation_weight_threshold", "min_llm_message_length"} {
		if _, exists := configKeys[required]; !exists {
			t.Fatalf("missing config field %q", required)
		}
	}
	for _, removed := range []string{"low_threshold", "high_threshold", "detection_mode"} {
		if _, exists := configKeys[removed]; exists {
			t.Fatalf("removed config field still exists: %q", removed)
		}
	}
}

func newManagementRuntime(t *testing.T) *plugin.RuntimeController {
	t.Helper()
	catalog, err := plugin.NewSpecCatalog([]plugin.PluginSpec{{
		Key: pluginKey, DisplayName: "违禁监控", Description: "测试运行门禁",
		Observers: []plugin.ObserverSpec{{Key: "test", Description: "测试观察入口", EventKinds: []plugin.ObserverEventKind{plugin.ObserverGroupMessage}, Handler: func(plugin.ObserverContext) error { return nil }}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := plugin.NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func TestManagementLiveOperationsRequireRuntimeAdmission(t *testing.T) {
	instance := testImplementation()
	repository := instance.repository.(*fakeMonitorRepository)
	repository.stored = storedViolation{ID: 7, Version: 1, Data: violationData{GroupID: 100, UserID: 200, Status: statusPendingReview, ActionResult: json.RawMessage(`{"ban_succeeded":false}`)}}
	repository.record = management.ResourceRecord{ID: 7, Version: 2, Data: json.RawMessage(`{"status":"confirmed_pending_kick"}`)}
	service := &Service{implementation: instance, authorizer: staticAuthorizer{}}
	controller := newManagementRuntime(t)
	if err := service.BindRuntimeController(controller); err != nil {
		t.Fatal(err)
	}

	if _, err := service.RunTextTrial(context.Background(), management.Actor{}, "测试"); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("disabled trial error = %v", err)
	}
	if _, err := service.ReviewViolation(context.Background(), management.Actor{}, 7, 1, "确认"); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("disabled review error = %v", err)
	}
	if err := controller.Enable(context.Background(), pluginKey); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunTextTrial(context.Background(), management.Actor{}, "测试"); err != nil {
		t.Fatalf("ready trial error = %v", err)
	}
	if _, err := service.ReviewViolation(context.Background(), management.Actor{}, 7, 1, "确认"); !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("closed group review error = %v", err)
	}
	if err := controller.SetGroupEnabled(pluginKey, 100, true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReviewViolation(context.Background(), management.Actor{}, 7, 1, "确认"); err != nil {
		t.Fatalf("enabled group review error = %v", err)
	}
}

func TestRefreshLexiconFailureFailsRuntimeClosed(t *testing.T) {
	instance := testImplementation()
	instance.lexiconStore = staticLexicon{err: errors.New("load failed")}
	service := &Service{implementation: instance, authorizer: staticAuthorizer{}}
	controller := newManagementRuntime(t)
	if err := service.BindRuntimeController(controller); err != nil {
		t.Fatal(err)
	}
	if err := controller.Enable(context.Background(), pluginKey); err != nil {
		t.Fatal(err)
	}
	if err := service.refreshLexicon(context.Background()); err == nil {
		t.Fatal("refreshLexicon() error = nil")
	}
	state, ok := controller.State(pluginKey)
	if !ok || state.Status != plugin.RuntimeDisabled {
		t.Fatalf("runtime state = %+v,%v", state, ok)
	}
}

func TestMaintenanceSkipsDisabledGroup(t *testing.T) {
	repository := &maintenanceRepository{fakeMonitorRepository: &fakeMonitorRepository{}}
	repository.groups = []int64{100}
	actions := &maintenanceActions{fakeActions: &fakeActions{}}
	instance := testImplementation()
	instance.repository = repository
	instance.actions = actions
	service := &Service{implementation: instance}
	controller := newManagementRuntime(t)
	if err := service.BindRuntimeController(controller); err != nil {
		t.Fatal(err)
	}
	if err := controller.Enable(context.Background(), pluginKey); err != nil {
		t.Fatal(err)
	}

	if err := instance.runMaintenance(context.Background()); err != nil {
		t.Fatalf("runMaintenance() error = %v", err)
	}
	if repository.listCalls != 1 || len(actions.memberListCalls) != 0 || len(repository.recentCalls) != 0 || len(repository.replaceCalls) != 0 {
		t.Fatalf("list=%d actions=%v recent=%v replacements=%v", repository.listCalls, actions.memberListCalls, repository.recentCalls, repository.replaceCalls)
	}
	state, ok := controller.State(pluginKey)
	if !ok || state.InFlight != 0 {
		t.Fatalf("runtime state after maintenance = %+v, %v", state, ok)
	}
}

func TestMaintenanceContinuesAfterGroupFailures(t *testing.T) {
	actionFailure := errors.New("group 100 unavailable")
	databaseFailure := errors.New("group 300 counts unavailable")
	writeFailure := errors.New("group 200 whitelist unavailable")
	repository := &maintenanceRepository{
		fakeMonitorRepository: &fakeMonitorRepository{},
		groups:                []int64{100, 200, 300, 400},
		recentErrors:          map[int64]error{300: databaseFailure},
		replacementError:      map[int64]error{200: writeFailure},
	}
	actions := &maintenanceActions{
		fakeActions:      &fakeActions{},
		memberListErrors: map[string]error{"100": actionFailure},
	}
	instance := testImplementation()
	instance.repository = repository
	instance.actions = actions
	service := &Service{implementation: instance}
	controller := newManagementRuntime(t)
	if err := service.BindRuntimeController(controller); err != nil {
		t.Fatal(err)
	}
	for _, groupID := range repository.groups {
		if err := controller.SetGroupEnabled(pluginKey, groupID, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := controller.Enable(context.Background(), pluginKey); err != nil {
		t.Fatal(err)
	}

	err := instance.runMaintenance(context.Background())
	if !errors.Is(err, actionFailure) || !errors.Is(err, databaseFailure) || !errors.Is(err, writeFailure) {
		t.Fatalf("runMaintenance() error = %v", err)
	}
	if got := strings.Join(actions.memberListCalls, ","); got != "100,200,300,400" {
		t.Fatalf("member list calls = %s", got)
	}
	if len(repository.recentCalls) != 3 || repository.recentCalls[0] != 200 || repository.recentCalls[1] != 300 || repository.recentCalls[2] != 400 {
		t.Fatalf("recent calls = %v", repository.recentCalls)
	}
	if len(repository.replaceCalls) != 2 || repository.replaceCalls[0] != 200 || repository.replaceCalls[1] != 400 {
		t.Fatalf("replace calls = %v", repository.replaceCalls)
	}
	state, ok := controller.State(pluginKey)
	if !ok || state.InFlight != 0 {
		t.Fatalf("runtime state after maintenance = %+v, %v", state, ok)
	}
}

func TestMaintenanceDeadlineAllowsRuntimeDisableToDrain(t *testing.T) {
	repository := &contextBlockingMaintenanceRepository{fakeMonitorRepository: &fakeMonitorRepository{}, started: make(chan struct{}, 2)}
	instance := testImplementation()
	instance.repository = repository
	instance.maintenanceTimeout = 200 * time.Millisecond
	service := &Service{implementation: instance}
	catalog, err := plugin.NewSpecCatalog([]plugin.PluginSpec{service.Spec()})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := plugin.NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.BindRuntimeController(controller); err != nil {
		t.Fatal(err)
	}
	if err := controller.Enable(context.Background(), pluginKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
		defer cleanupCancel()
		_ = controller.Disable(cleanupContext, pluginKey)
	})
	maintenanceResult := make(chan error, 1)
	go func() {
		maintenanceContext := context.WithValue(context.Background(), maintenanceContextKey{}, true)
		maintenanceResult <- instance.runMaintenance(maintenanceContext)
	}()
	select {
	case <-repository.started:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not acquire runtime admission")
	}
	state, ok := controller.State(pluginKey)
	if !ok || state.InFlight < 1 {
		t.Fatalf("runtime state before disable = %+v, %v", state, ok)
	}
	disableContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.Disable(disableContext, pluginKey); err != nil {
		t.Fatalf("Disable() error = %v", err)
	}
	if err := <-maintenanceResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runMaintenance() error = %v", err)
	}
	state, ok = controller.State(pluginKey)
	if !ok || state.Status != plugin.RuntimeDisabled || state.InFlight != 0 {
		t.Fatalf("runtime state after disable = %+v, %v", state, ok)
	}
	instance.lifecycleMu.Lock()
	workerCleared := instance.cancel == nil && instance.done == nil
	instance.lifecycleMu.Unlock()
	if !workerCleared {
		t.Fatal("maintenance worker was not released")
	}
}

func TestMaintenanceDeadlineRotatesToLaterGroups(t *testing.T) {
	repository := &maintenanceRepository{fakeMonitorRepository: &fakeMonitorRepository{}, groups: []int64{100, 200}}
	actions := &maintenanceActions{fakeActions: &fakeActions{}, waitForCancel: map[string]bool{"100": true}}
	instance := testImplementation()
	instance.repository = repository
	instance.actions = actions
	instance.maintenanceTimeout = 20 * time.Millisecond
	service := &Service{implementation: instance}
	controller := newManagementRuntime(t)
	if err := service.BindRuntimeController(controller); err != nil {
		t.Fatal(err)
	}
	for _, groupID := range repository.groups {
		if err := controller.SetGroupEnabled(pluginKey, groupID, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := controller.Enable(context.Background(), pluginKey); err != nil {
		t.Fatal(err)
	}

	if err := instance.runMaintenance(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first runMaintenance() error = %v", err)
	}
	if err := instance.runMaintenance(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second runMaintenance() error = %v", err)
	}
	if got := strings.Join(actions.memberListCalls, ","); got != "100,200,100" {
		t.Fatalf("member list calls = %s", got)
	}
	if len(repository.replaceCalls) != 1 || repository.replaceCalls[0] != 200 {
		t.Fatalf("replace calls = %v", repository.replaceCalls)
	}
}

func TestMaintenanceRejectsEnablingRuntimeAndUsesRetryInterval(t *testing.T) {
	lifecycle := &blockingEnableLifecycle{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-lifecycle.release:
		default:
			close(lifecycle.release)
		}
	})
	catalog, err := plugin.NewSpecCatalog([]plugin.PluginSpec{{
		Key: pluginKey, DisplayName: "违禁监控", Description: "测试 enabling 门禁", Lifecycle: lifecycle,
	}})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := plugin.NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	repository := &maintenanceRepository{fakeMonitorRepository: &fakeMonitorRepository{}, groups: []int64{100}}
	instance := testImplementation()
	instance.repository = repository
	service := &Service{implementation: instance}
	if err := service.BindRuntimeController(nil); err == nil {
		t.Fatal("BindRuntimeController(nil) error = nil")
	}
	if err := service.BindRuntimeController(controller); err != nil {
		t.Fatal(err)
	}
	if instance.runtimeGate != controller {
		t.Fatal("BindRuntimeController() did not inject maintenance gate")
	}
	enableResult := make(chan error, 1)
	go func() {
		enableResult <- controller.Enable(context.Background(), pluginKey)
	}()
	select {
	case <-lifecycle.started:
	case <-time.After(time.Second):
		t.Fatal("runtime did not enter enabling")
	}
	state, ok := controller.State(pluginKey)
	if !ok || state.Status != plugin.RuntimeEnabling {
		t.Fatalf("runtime state = %+v, %v", state, ok)
	}

	err = instance.runMaintenance(context.Background())
	if err == nil {
		t.Fatal("runMaintenance() error = nil while enabling")
	}
	if repository.listCalls != 0 {
		t.Fatalf("ListObservedGroups() calls = %d", repository.listCalls)
	}
	if got := nextMaintenanceInterval(err); got != maintenanceRetryInterval {
		t.Fatalf("nextMaintenanceInterval(error) = %v", got)
	}
	if got := nextMaintenanceInterval(nil); got != maintenanceInterval {
		t.Fatalf("nextMaintenanceInterval(nil) = %v", got)
	}
	close(lifecycle.release)
	if err := <-enableResult; err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
}

// TestConstructorDependencies 验证构造依赖边界。
// @param t：Go测试上下文。
// @returns 无。
// ⚠️副作用说明：仅分配内存实例。
func TestConstructorDependencies(t *testing.T) {
	_, err := newImplementation(nil, nil)
	// [决策理由] 自动处置插件缺少Action和数据库时必须拒绝启动。
	if err == nil {
		t.Fatal("newImplementation() missing dependencies error=nil")
	}
	instance, err := newImplementation(&fakeActions{}, &pgxpool.Pool{})
	// [决策理由] 构造成功时必须返回可注册的非空实现。
	if err != nil || instance == nil {
		t.Fatalf("newImplementation() instance/error = %v/%v", instance, err)
	}
	// >>> 数据演变示例
	// 1. Runtime{} -> 缺依赖错误。
	// 2. Runtime{Actions,Database} -> implementation且Name匹配。
}

// TestLifecycleIsIdempotent 验证无资源生命周期可安全重复调用。
// @param t：Go测试上下文。
// @returns 无。
// ⚠️副作用说明：仅调用无状态实例的生命周期方法。
// staticLexicon 是测试用的内存词库来源。
type staticLexicon struct {
	lexicon Lexicon
	err     error
}

func (s staticLexicon) Load(context.Context) (Lexicon, error) { return s.lexicon, s.err }

func TestLifecycleIsIdempotent(t *testing.T) {
	instance := testImplementation()
	for call := 0; call < 2; call++ {
		// [决策理由] 启用可能由启动恢复和热切换重复触发，必须保持幂等。
		if err := instance.OnEnable(context.Background()); err != nil {
			t.Fatalf("OnEnable() call %d error = %v", call+1, err)
		}
		// [决策理由] 禁用也可能在资源已释放后重复触发，必须安全返回。
		if err := instance.OnDisable(context.Background()); err != nil {
			t.Fatalf("OnDisable() call %d error = %v", call+1, err)
		}
	}

	// >>> 数据演变示例
	// 1. enable->disable -> 均无资源变更 -> nil。
	// 2. 再次enable->disable -> 状态不累积 -> nil。
}

// TestDisableTimeoutKeepsWorkerGeneration 验证停用超时不会遗失旧worker句柄或启动第二个worker。
// @param t：Go测试上下文。
// @returns 无。
// ⚠️副作用说明：启动并取消一个受控阻塞的维护协程。
func TestDisableTimeoutKeepsWorkerGeneration(t *testing.T) {
	repository := &blockingMaintenanceRepository{fakeMonitorRepository: &fakeMonitorRepository{}, started: make(chan struct{}), release: make(chan struct{})}
	instance := testImplementation()
	instance.repository = repository
	controller := newManagementRuntime(t)
	if err := controller.Enable(context.Background(), pluginKey); err != nil {
		t.Fatal(err)
	}
	instance.runtimeGate = controller
	// [决策理由] 启用不能等待首次维护，否则NapCat尚未连接时会阻塞服务启动。
	if err := instance.OnEnable(context.Background()); err != nil {
		t.Fatalf("OnEnable() error = %v", err)
	}
	select {
	case <-repository.started:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not start")
	}
	instance.lifecycleMu.Lock()
	firstDone := instance.done
	instance.lifecycleMu.Unlock()
	disableContext, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	// [决策理由] 外部维护尚未返回时停用必须按调用方截止时间返回，同时保留生命周期代次。
	if err := instance.OnDisable(disableContext); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("OnDisable() error = %v, want deadline exceeded", err)
	}
	// [决策理由] 超时后的重复启用必须记录恢复意图，但不能在旧代次退出前创建第二个worker。
	if err := instance.OnEnable(context.Background()); err != nil {
		t.Fatalf("second OnEnable() error = %v", err)
	}
	instance.lifecycleMu.Lock()
	secondDone := instance.done
	instance.lifecycleMu.Unlock()
	// [决策理由] done通道身份代表worker代次，变化即说明错误启动了第二个worker。
	if secondDone != firstDone {
		t.Fatal("OnEnable() replaced worker after disable timeout")
	}
	close(repository.release)
	deadline := time.After(time.Second)
	for {
		instance.lifecycleMu.Lock()
		restartedDone := instance.done
		instance.lifecycleMu.Unlock()
		// [决策理由] 停用失败后管理器仍视为启用，旧代次退出必须自动恢复为新worker。
		if restartedDone != nil && restartedDone != firstDone {
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker did not restart after failed disable")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
	defer cleanupCancel()
	// [决策理由] 旧worker退出后再次停用必须能够继续等待并清理保留的句柄。
	if err := instance.OnDisable(cleanupContext); err != nil {
		t.Fatalf("cleanup OnDisable() error = %v", err)
	}
	instance.lifecycleMu.Lock()
	workerCleared := instance.cancel == nil && instance.done == nil
	instance.lifecycleMu.Unlock()
	// [决策理由] 成功等待退出后必须清空句柄，允许未来重新启用。
	if !workerCleared {
		t.Fatal("worker handles were not cleared")
	}

	// >>> 数据演变示例
	// 1. enable->维护阻塞->disable超时->enable -> 旧代次退出前仍为同一done。
	// 2. release->自动新代次->disable成功 -> cancel/done清空。
}

// TestHandleIgnoresUnsupportedEvents 验证私聊、机器人自发消息和元事件安静返回。
// @param t：Go测试上下文。
// @returns 无。
// ⚠️副作用说明：无。
func TestHandleIgnoresUnsupportedEvents(t *testing.T) {
	instance := testImplementation()
	events := []ws.Event{
		&ws.MessageEvent{BaseEvent: ws.BaseEvent{PostType: "message"}, MessageType: "private", UserID: 1, RawMessage: "私聊"},
		&ws.MessageEvent{BaseEvent: ws.BaseEvent{PostType: "message_sent"}, MessageType: "group", GroupID: 100, UserID: 1, RawMessage: "机器人消息"},
		&ws.HeartbeatEvent{BaseEvent: ws.BaseEvent{PostType: "meta_event"}, MetaEventType: "heartbeat"},
	}
	for _, event := range events {
		// [决策理由] 非入站群消息和无关元事件不得进入计数、检测或处置。
		if err := dispatchEvent(instance, context.Background(), event); err != nil {
			t.Fatalf("Handle(%T) error = %v", event, err)
		}
	}

	// >>> 数据演变示例
	// 1. private/message_sent -> nil且不访问仓储。
	// 2. heartbeat -> nil且不访问Action。
}

func TestZeroScoreLongMessageUsesLLM(t *testing.T) {
	instance := testImplementation()
	evaluator := &fakeLLMEvaluator{result: LLMEvaluationResult{RiskLevel: "Low", TotalScore: 5, Reason: "普通交流", Violations: []string{}, SuggestedAction: "pass"}}
	current := instance.snapshot.Load()
	instance.snapshot.Store(&runtimeSnapshot{engine: current.engine, engineConfig: current.engineConfig, evaluator: evaluator, llmTimeout: time.Second, llmMaxConcurrency: 2, llmDailyRequestLimit: 500, minLLMMessageLength: 8})
	event := &ws.MessageEvent{BaseEvent: ws.BaseEvent{PostType: "message", Time: time.Now().Unix()}, MessageType: "group", GroupID: 100, UserID: 200, MessageID: 9, RawMessage: "这 是、尚未收录的新型内容[CQ:image,file=secret.jpg]"}
	if err := dispatchEvent(instance, context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if evaluator.calls != 1 || len(evaluator.requests) != 1 || evaluator.requests[0].Message != "这是尚未收录的新型内容" {
		t.Fatalf("calls=%d requests=%+v", evaluator.calls, evaluator.requests)
	}
	if created := instance.repository.(*fakeMonitorRepository).created; len(created) != 0 {
		t.Fatalf("pass result created violations: %+v", created)
	}
}

func TestShortZeroScoreAndHighLocalScoreDoNotUseLLM(t *testing.T) {
	instance := testImplementation()
	evaluator := &fakeLLMEvaluator{result: LLMEvaluationResult{RiskLevel: "Low", TotalScore: 5, Reason: "普通交流", Violations: []string{}, SuggestedAction: "pass"}}
	config := EngineConfig{ViolationThreshold: 60, WeightedKeywords: []WeightedKeyword{{Text: "短广告", Weight: 60}}}
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}
	instance.snapshot.Store(&runtimeSnapshot{engine: engine, engineConfig: config, evaluator: evaluator, llmTimeout: time.Second, llmMaxConcurrency: 2, llmDailyRequestLimit: 500, minLLMMessageLength: 30})
	short := &ws.MessageEvent{BaseEvent: ws.BaseEvent{PostType: "message", Time: time.Now().Unix()}, MessageType: "group", GroupID: 100, UserID: 200, MessageID: 10, RawMessage: "普通短消息"}
	if err := dispatchEvent(instance, context.Background(), short); err != nil {
		t.Fatal(err)
	}
	high := &ws.MessageEvent{BaseEvent: ws.BaseEvent{PostType: "message", Time: time.Now().Unix()}, MessageType: "group", GroupID: 100, UserID: 201, MessageID: 11, RawMessage: "短、广 告"}
	if err := dispatchEvent(instance, context.Background(), high); err != nil {
		t.Fatal(err)
	}
	if evaluator.calls != 0 || len(instance.actions.(*fakeActions).banParams) != 1 {
		t.Fatalf("calls=%d bans=%d", evaluator.calls, len(instance.actions.(*fakeActions).banParams))
	}
}

func TestLLMReceivesCleanedTextAndAuditKeepsRawMessage(t *testing.T) {
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	repository := &fakeMonitorRepository{}
	actions := &fakeActions{}
	evaluator := &fakeLLMEvaluator{result: LLMEvaluationResult{RiskLevel: "High", TotalScore: 90, Reason: "新型引流", Violations: []string{"新型引流"}, SuggestedAction: "block"}}
	config := DefaultEngineConfig()
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}
	instance := &implementation{actions: actions, repository: repository, now: func() time.Time { return now }}
	instance.snapshot.Store(&runtimeSnapshot{engine: engine, engineConfig: config, evaluator: evaluator, llmTimeout: time.Second, llmMaxConcurrency: 2, llmDailyRequestLimit: 500, minLLMMessageLength: 4})
	raw := "新 型、引\n流内容[CQ:image,file=secret.jpg]"
	event := &ws.MessageEvent{BaseEvent: ws.BaseEvent{PostType: "message", Time: now.Unix()}, MessageType: "group", GroupID: 100, UserID: 200, MessageID: 12, MessageSeq: 222, RawMessage: raw}
	if err := dispatchEvent(instance, context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if len(evaluator.requests) != 1 || evaluator.requests[0].Message != "新型引流内容" {
		t.Fatalf("requests = %+v", evaluator.requests)
	}
	if len(repository.created) != 1 || repository.created[0].MessageContent != raw || repository.created[0].DetectionSource != "llm" {
		t.Fatalf("created = %+v", repository.created)
	}
}

// TestHandleWeightedViolationModeratesAndRecords 验证本地累计权重执行禁言、筛选撤回和审计。
// @param t：Go测试上下文。
// @returns 无。
// ⚠️副作用说明：修改fake Action与仓储记录。
func TestHandleWeightedViolationModeratesAndRecords(t *testing.T) {
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	repository := &fakeMonitorRepository{}
	actions := &fakeActions{history: onebot.GetGroupMessageHistoryResult{Messages: []onebot.GroupHistoryMessage{
		{Time: now.Unix(), MessageID: 9, UserID: 200},
		{Time: now.Add(-time.Minute).Unix(), MessageID: 8, UserID: 200},
		{Time: now.Unix(), MessageID: 7, UserID: 300},
		{Time: now.Add(time.Minute).Unix(), MessageID: 10, UserID: 200},
	}}}
	config := EngineConfig{ViolationThreshold: 60, WeightedKeywords: []WeightedKeyword{{Text: "内部渠道", Weight: 60}}}
	engine, err := NewEngine(config)
	// [决策理由] 测试规则必须成功构造才能验证处置链路。
	if err != nil {
		t.Fatal(err)
	}
	instance := &implementation{actions: actions, repository: repository, now: func() time.Time { return now }}
	instance.snapshot.Store(&runtimeSnapshot{engine: engine, engineConfig: config, llmTimeout: time.Second})
	event := &ws.MessageEvent{BaseEvent: ws.BaseEvent{PostType: "message", Time: now.Unix()}, MessageType: "group", GroupID: 100, UserID: 200, MessageID: 9, MessageSeq: 222, RawMessage: "内部渠道"}
	err = dispatchEvent(instance, context.Background(), event)
	// [决策理由] 成功处置和审计后事件允许后续插件继续处理且不返回错误。
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	// [决策理由] 禁言必须固定30天且精确定位群与用户。
	if len(actions.banParams) != 1 || actions.banParams[0].Duration != banDurationSeconds || actions.banParams[0].GroupID != "100" || actions.banParams[0].UserID != "200" {
		t.Fatalf("ban params = %+v", actions.banParams)
	}
	// [决策理由] 仅撤回锚点前同一用户的两条消息，其他用户和未来消息必须保留。
	if len(actions.deleted) != 2 || actions.deleted[0] != int64(9) || actions.deleted[1] != int64(8) {
		t.Fatalf("deleted = %+v", actions.deleted)
	}
	// [决策理由] 历史页必须以违规消息序号为锚点，不能读取执行时的最新消息页。
	if len(actions.historyParams) != 1 || actions.historyParams[0].MessageSeq != "222" {
		t.Fatalf("history params = %+v", actions.historyParams)
	}
	// [决策理由] 自动动作必须对应唯一待复核证据。
	if len(repository.created) != 1 || repository.created[0].DetectionSource != "weighted_score" || repository.created[0].MessageContent != "内部渠道" {
		t.Fatalf("created = %+v", repository.created)
	}

	// >>> 数据演变示例
	// 1. 本地权重达阈值+历史含本人2条 -> 禁言30天+撤回2条+审计1条。
	// 2. 他人消息或锚点后消息 -> 筛选跳过。
}

// TestHandleWeightedViolationIsIdempotent 验证重复事件只执行一次外部处罚。
// @param t：Go测试上下文。
// @returns 无。
// ⚠️副作用说明：修改fake仓储与Action调用记录。
func TestHandleWeightedViolationIsIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	repository := &fakeMonitorRepository{}
	actions := &fakeActions{}
	config := EngineConfig{ViolationThreshold: 60, WeightedKeywords: []WeightedKeyword{{Text: "固定广告", Weight: 60}}}
	engine, err := NewEngine(config)
	// [决策理由] 规则构造失败时无法验证重复处置链路。
	if err != nil {
		t.Fatal(err)
	}
	instance := &implementation{actions: actions, repository: repository, now: func() time.Time { return now }}
	instance.snapshot.Store(&runtimeSnapshot{engine: engine, engineConfig: config, llmTimeout: time.Second})
	event := &ws.MessageEvent{BaseEvent: ws.BaseEvent{PostType: "message", Time: now.Unix()}, MessageType: "group", GroupID: 100, UserID: 200, MessageID: 9, MessageSeq: 222, RawMessage: "固定广告"}
	for call := 0; call < 2; call++ {
		// [决策理由] OneBot可能重复投递同一message_id，两次处理都应安全返回。
		if err := dispatchEvent(instance, context.Background(), event); err != nil {
			t.Fatalf("Handle() call%d error=%v", call+1, err)
		}
	}
	// [决策理由] 第二次预留冲突必须在禁言、历史和撤回之前短路。
	if repository.reserveCalls != 2 || repository.completeCalls != 1 || len(actions.banParams) != 1 || len(actions.historyParams) != 1 {
		t.Fatalf("reserve=%d complete=%d bans=%d history=%d", repository.reserveCalls, repository.completeCalls, len(actions.banParams), len(actions.historyParams))
	}
	// >>> 数据演变示例
	// 1. 首次message9 -> 预留成功 -> 禁言与审计完成。
	// 2. 重复message9 -> 预留未获得 -> 无第二次Action。
}

// TestHandleWeightedViolationSkipsActionsWhenReservationFails 验证数据库预留失败时不处罚。
// @param t：Go测试上下文。
// @returns 无。
// ⚠️副作用说明：读取fake Action调用记录。
func TestHandleWeightedViolationSkipsActionsWhenReservationFails(t *testing.T) {
	repository := &fakeMonitorRepository{reserveErr: errors.New("db unavailable")}
	actions := &fakeActions{}
	config := EngineConfig{ViolationThreshold: 60, WeightedKeywords: []WeightedKeyword{{Text: "固定广告", Weight: 60}}}
	engine, err := NewEngine(config)
	// [决策理由] 测试必须先构造可命中的确定性规则。
	if err != nil {
		t.Fatal(err)
	}
	instance := &implementation{actions: actions, repository: repository, now: time.Now}
	instance.snapshot.Store(&runtimeSnapshot{engine: engine, engineConfig: config, llmTimeout: time.Second})
	event := &ws.MessageEvent{BaseEvent: ws.BaseEvent{PostType: "message"}, MessageType: "group", GroupID: 100, UserID: 200, MessageID: 9, MessageSeq: 222, RawMessage: "固定广告"}
	err = dispatchEvent(instance, context.Background(), event)
	// [决策理由] 无法落审计时必须返回错误且保持QQ外部状态不变。
	if err == nil || len(actions.banParams) != 0 || len(actions.historyParams) != 0 || len(actions.deleted) != 0 {
		t.Fatalf("Handle() error=%v bans=%d history=%d deleted=%d", err, len(actions.banParams), len(actions.historyParams), len(actions.deleted))
	}
	// >>> 数据演变示例
	// 1. Reserve成功 -> 可进入Action（由其他测试覆盖）。
	// 2. Reserve数据库失败 -> 返回error且Action计数全0。
}

// TestHandleWhitelistBypassesDetection 验证白名单用户只计数而不运行处罚。
// @param t：Go测试上下文。
// @returns 无。
// ⚠️副作用说明：读取fake Action与仓储记录。
func TestHandleWhitelistBypassesDetection(t *testing.T) {
	instance := testImplementation()
	repository := instance.repository.(*fakeMonitorRepository)
	repository.whitelisted = true
	evaluator := &fakeLLMEvaluator{result: LLMEvaluationResult{RiskLevel: "High", TotalScore: 90, Reason: "违规", Violations: []string{"广告"}, SuggestedAction: "block"}}
	config := EngineConfig{ViolationThreshold: 60, WeightedKeywords: []WeightedKeyword{{Text: "广告", Weight: 100}}}
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}
	instance.snapshot.Store(&runtimeSnapshot{engine: engine, engineConfig: config, evaluator: evaluator, llmTimeout: time.Second, llmMaxConcurrency: 2, llmDailyRequestLimit: 500, minLLMMessageLength: 1})
	event := &ws.MessageEvent{BaseEvent: ws.BaseEvent{PostType: "message"}, MessageType: "group", GroupID: 100, UserID: 200, MessageID: 9, RawMessage: "高风险广告"}
	err = dispatchEvent(instance, context.Background(), event)
	// [决策理由] 白名单必须在任何硬词或评分检测前直接放行。
	if err != nil || repository.validSpeechCalls != 1 || evaluator.calls != 0 || len(instance.actions.(*fakeActions).banParams) != 0 || len(repository.created) != 0 {
		t.Fatalf("Handle() error=%v speeches=%d llm=%d bans=%+v audits=%+v", err, repository.validSpeechCalls, evaluator.calls, instance.actions.(*fakeActions).banParams, repository.created)
	}

	// >>> 数据演变示例
	// 1. whitelist=true+任意文本 -> 无禁言无审计。
	// 2. whitelist=false -> 其他测试验证进入精准检测。
}

// staticAuthorizer 是测试用的放行授权器。
type staticAuthorizer struct{}

func (staticAuthorizer) Authorize(management.Actor) error { return nil }

// dispatchEvent 复用观察器的事件路由，替代已移除的广播 Handle。
func dispatchEvent(instance *implementation, ctx context.Context, event ws.Event) error {
	service := &Service{implementation: instance, authorizer: staticAuthorizer{}}
	groupID := int64(0)
	switch typed := event.(type) {
	case *ws.MessageEvent:
		groupID = typed.GroupID
	case *ws.GroupBanNotice:
		groupID = typed.GroupID
	case *ws.NoticeEvent:
		groupID = typed.GroupID
	}
	return service.observe(plugin.ObserverContext{Context: ctx, GroupID: groupID, Event: event})
}
