// 📌 影响范围：执行 Argon2id 哈希；使用内存 HTTP 测试请求，不访问数据库或网络。
package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/w1ndys-bot/internal/admin"
	"github.com/w1ndys/w1ndys-bot/internal/management"
	"github.com/w1ndys/w1ndys-bot/internal/onebot"
	"github.com/w1ndys/w1ndys-bot/internal/plugin"
)

type fakeRuntimeStates struct {
	states        []plugin.RuntimeStateView
	state         plugin.RuntimeStateView
	config        plugin.RuntimeConfigView
	err           error
	actor         management.Actor
	key           string
	enabled       bool
	groupID       int64
	version       int64
	payload       json.RawMessage
	calls         []string
	copyPreview   plugin.RuntimeGroupCopyPreview
	copyResult    plugin.RuntimeGroupCopyResult
	staleStates   []plugin.RuntimeStaleGroupState
	copySource    int64
	copyTarget    int64
	copyStates    []plugin.RuntimeGroupCopyItem
	cleanupStates []plugin.RuntimeStaleGroupState
}

func (f *fakeRuntimeStates) PreviewGroupCopy(_ context.Context, actor management.Actor, sourceGroupID, targetGroupID int64) (plugin.RuntimeGroupCopyPreview, error) {
	f.actor, f.copySource, f.copyTarget = actor, sourceGroupID, targetGroupID
	f.calls = append(f.calls, "preview-copy")
	return f.copyPreview, f.err
}

func (f *fakeRuntimeStates) CopyGroupStates(_ context.Context, actor management.Actor, sourceGroupID, targetGroupID int64, states []plugin.RuntimeGroupCopyItem) (plugin.RuntimeGroupCopyResult, error) {
	f.actor, f.copySource, f.copyTarget, f.copyStates = actor, sourceGroupID, targetGroupID, states
	f.calls = append(f.calls, "copy")
	return f.copyResult, f.err
}

func (f *fakeRuntimeStates) PreviewStaleGroupStates(_ context.Context, actor management.Actor) ([]plugin.RuntimeStaleGroupState, error) {
	f.actor, f.calls = actor, append(f.calls, "preview-stale")
	return f.staleStates, f.err
}

func (f *fakeRuntimeStates) CleanupStaleGroupStates(_ context.Context, actor management.Actor, states []plugin.RuntimeStaleGroupState) ([]plugin.RuntimeStaleGroupState, error) {
	f.actor, f.cleanupStates = actor, states
	f.calls = append(f.calls, "cleanup-stale")
	return f.staleStates, f.err
}

func (f *fakeRuntimeStates) GetConfig(_ context.Context, actor management.Actor, key string) (plugin.RuntimeConfigView, error) {
	f.actor, f.key, f.calls = actor, key, append(f.calls, "get-config")
	return f.config, f.err
}

func (f *fakeRuntimeStates) SetConfig(_ context.Context, actor management.Actor, key string, config json.RawMessage, version int64) (plugin.RuntimeConfigView, error) {
	f.actor, f.key, f.payload, f.version = actor, key, config, version
	f.calls = append(f.calls, "set-config")
	return f.config, f.err
}

func (f *fakeRuntimeStates) List(_ context.Context, actor management.Actor) ([]plugin.RuntimeStateView, error) {
	f.actor, f.calls = actor, append(f.calls, "list")
	return f.states, f.err
}

func (f *fakeRuntimeStates) Get(_ context.Context, actor management.Actor, key string) (plugin.RuntimeStateView, error) {
	f.actor, f.key, f.calls = actor, key, append(f.calls, "get")
	return f.state, f.err
}

func (f *fakeRuntimeStates) SetGlobalEnabled(_ context.Context, actor management.Actor, key string, enabled bool, version int64) (plugin.RuntimeStateView, error) {
	f.actor, f.key, f.enabled, f.version = actor, key, enabled, version
	f.calls = append(f.calls, "global")
	return f.state, f.err
}

func (f *fakeRuntimeStates) SetGroupEnabled(_ context.Context, actor management.Actor, key string, groupID int64, enabled bool, version int64) (plugin.RuntimeStateView, error) {
	f.actor, f.key, f.groupID, f.enabled, f.version = actor, key, groupID, enabled, version
	f.calls = append(f.calls, "group")
	return f.state, f.err
}

func newRuntimeTestServer(t *testing.T, runtimes *fakeRuntimeStates) (*Server, string) {
	t.Helper()
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Enabled: true}}}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, &fakePlugins{}, runtimes, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	if err != nil {
		t.Fatal(err)
	}
	server.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	token, err := server.sign("100")
	if err != nil {
		t.Fatal(err)
	}
	return server, token
}

func runtimeRequest(t *testing.T, server *Server, token, method, path, body, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Request-ID", requestID)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestPluginRuntimeRoutesReadAndWrite(t *testing.T) {
	runtimes := &fakeRuntimeStates{
		states: []plugin.RuntimeStateView{{
			PluginKey: "echo", DisplayName: "Echo 回声", DesiredEnabled: true, Version: 2,
			Status: plugin.RuntimeReady, Groups: []plugin.RuntimeGroupView{{GroupID: 100, Enabled: true, Version: 1}},
		}},
		state: plugin.RuntimeStateView{PluginKey: "echo", DesiredEnabled: true, Version: 3, Status: plugin.RuntimeReady},
	}
	server, token := newRuntimeTestServer(t, runtimes)

	listRecorder := runtimeRequest(t, server, token, http.MethodGet, "/api/plugin-runtimes", "", "req-list")
	// 列表必须同时暴露管理员意图和进程内实际状态。
	body := listRecorder.Body.String()
	if listRecorder.Code != http.StatusOK || !strings.Contains(body, `"desired_enabled":true`) || !strings.Contains(body, `"status":"ready"`) {
		t.Fatalf("list status=%d body=%s", listRecorder.Code, body)
	}
	if !strings.Contains(body, `"group_id":100`) || runtimes.actor.ID != "100" || runtimes.actor.RequestID != "req-list" {
		t.Fatalf("list body=%s actor=%+v", body, runtimes.actor)
	}

	getRecorder := runtimeRequest(t, server, token, http.MethodGet, "/api/plugin-runtimes/echo", "", "req-get")
	if getRecorder.Code != http.StatusOK || runtimes.key != "echo" {
		t.Fatalf("get status=%d key=%s", getRecorder.Code, runtimes.key)
	}

	patchRecorder := runtimeRequest(t, server, token, http.MethodPatch, "/api/plugin-runtimes/echo", `{"enabled":true,"expected_version":2}`, "req-patch")
	if patchRecorder.Code != http.StatusOK || !runtimes.enabled || runtimes.version != 2 || runtimes.actor.RequestID != "req-patch" {
		t.Fatalf("patch status=%d fake=%+v", patchRecorder.Code, runtimes)
	}

	putRecorder := runtimeRequest(t, server, token, http.MethodPut, "/api/plugin-runtimes/echo/groups/100", `{"enabled":false,"expected_version":0}`, "req-group")
	if putRecorder.Code != http.StatusOK || runtimes.groupID != 100 || runtimes.enabled || runtimes.version != 0 {
		t.Fatalf("put status=%d fake=%+v", putRecorder.Code, runtimes)
	}
	// 管理通道必须固定为 webui，不能由请求体伪造。
	if runtimes.actor.Channel != management.ChannelWebUI {
		t.Fatalf("actor channel = %q", runtimes.actor.Channel)
	}
}

func TestPluginRuntimeRoutesRejectInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "全局缺少版本", method: http.MethodPatch, path: "/api/plugin-runtimes/echo", body: `{"enabled":true}`},
		{name: "全局零版本", method: http.MethodPatch, path: "/api/plugin-runtimes/echo", body: `{"enabled":true,"expected_version":0}`},
		{name: "未知字段", method: http.MethodPatch, path: "/api/plugin-runtimes/echo", body: `{"enabled":true,"expected_version":1,"extra":1}`},
		{name: "群负版本", method: http.MethodPut, path: "/api/plugin-runtimes/echo/groups/100", body: `{"enabled":true,"expected_version":-1}`},
		{name: "群号非法", method: http.MethodPut, path: "/api/plugin-runtimes/echo/groups/0", body: `{"enabled":true,"expected_version":0}`},
		{name: "群号非数字", method: http.MethodPut, path: "/api/plugin-runtimes/echo/groups/abc", body: `{"enabled":true,"expected_version":0}`},
		{name: "配置缺少版本", method: http.MethodPut, path: "/api/plugin-runtimes/echo/config", body: `{"config":{}}`},
		{name: "配置缺少对象", method: http.MethodPut, path: "/api/plugin-runtimes/echo/config", body: `{"expected_version":1}`},
		{name: "配置未知字段", method: http.MethodPut, path: "/api/plugin-runtimes/echo/config", body: `{"config":{},"expected_version":1,"extra":1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtimes := &fakeRuntimeStates{}
			server, token := newRuntimeTestServer(t, runtimes)
			recorder := runtimeRequest(t, server, token, test.method, test.path, test.body, "req-invalid")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			// 输入校验失败不得触达服务层。
			if len(runtimes.calls) != 0 {
				t.Fatalf("service called: %v", runtimes.calls)
			}
		})
	}
}

func TestPluginRuntimeGroupMaintenanceRoutes(t *testing.T) {
	runtimes := &fakeRuntimeStates{
		copyPreview: plugin.RuntimeGroupCopyPreview{
			Source: onebot.GroupInfo{GroupID: 100, GroupName: "源群"},
			Target: onebot.GroupInfo{GroupID: 200, GroupName: "目标群"},
			States: []plugin.RuntimeGroupCopyItem{{PluginKey: "echo", Enabled: true, SourceVersion: 2, TargetVersion: 0}},
		},
		copyResult:  plugin.RuntimeGroupCopyResult{SourceGroupID: 100, TargetGroupID: 200},
		staleStates: []plugin.RuntimeStaleGroupState{{PluginKey: "echo", GroupID: 300, Enabled: true, Version: 4}},
	}
	server, token := newRuntimeTestServer(t, runtimes)

	previewCopy := runtimeRequest(t, server, token, http.MethodPost, "/api/plugin-runtimes/group-maintenance/copy/preview", `{"source_group_id":100,"target_group_id":200}`, "req-copy-preview")
	if previewCopy.Code != http.StatusOK || runtimes.copySource != 100 || runtimes.copyTarget != 200 || !strings.Contains(previewCopy.Body.String(), `"group_name":"源群"`) {
		t.Fatalf("copy preview status=%d body=%s fake=%+v", previewCopy.Code, previewCopy.Body.String(), runtimes)
	}

	copyBody := `{"source_group_id":100,"target_group_id":200,"states":[{"plugin_key":"echo","enabled":true,"source_version":2,"target_version":0}]}`
	copyResult := runtimeRequest(t, server, token, http.MethodPost, "/api/plugin-runtimes/group-maintenance/copy", copyBody, "req-copy")
	if copyResult.Code != http.StatusOK || len(runtimes.copyStates) != 1 || runtimes.copyStates[0].PluginKey != "echo" || runtimes.actor.RequestID != "req-copy" {
		t.Fatalf("copy status=%d body=%s fake=%+v", copyResult.Code, copyResult.Body.String(), runtimes)
	}

	previewStale := runtimeRequest(t, server, token, http.MethodPost, "/api/plugin-runtimes/group-maintenance/stale/preview", "", "req-stale-preview")
	if previewStale.Code != http.StatusOK || !strings.Contains(previewStale.Body.String(), `"group_id":300`) {
		t.Fatalf("stale preview status=%d body=%s", previewStale.Code, previewStale.Body.String())
	}

	cleanupBody := `{"states":[{"plugin_key":"echo","group_id":300,"expected_version":4}]}`
	cleanup := runtimeRequest(t, server, token, http.MethodPost, "/api/plugin-runtimes/group-maintenance/stale/cleanup", cleanupBody, "req-cleanup")
	if cleanup.Code != http.StatusOK || len(runtimes.cleanupStates) != 1 {
		t.Fatalf("cleanup status=%d body=%s fake=%+v", cleanup.Code, cleanup.Body.String(), runtimes)
	}
	item := runtimes.cleanupStates[0]
	if item.PluginKey != "echo" || item.GroupID != 300 || item.Version != 4 || item.Enabled || !item.UpdatedAt.IsZero() {
		t.Fatalf("cleanup service item=%+v", item)
	}
}

func TestPluginRuntimeGroupMaintenanceRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "复制同群", path: "/api/plugin-runtimes/group-maintenance/copy/preview", body: `{"source_group_id":100,"target_group_id":100}`},
		{name: "复制未知字段", path: "/api/plugin-runtimes/group-maintenance/copy/preview", body: `{"source_group_id":100,"target_group_id":200,"extra":true}`},
		{name: "复制空状态", path: "/api/plugin-runtimes/group-maintenance/copy", body: `{"source_group_id":100,"target_group_id":200,"states":[]}`},
		{name: "复制重复插件", path: "/api/plugin-runtimes/group-maintenance/copy", body: `{"source_group_id":100,"target_group_id":200,"states":[{"plugin_key":"echo","enabled":true,"source_version":1,"target_version":0},{"plugin_key":"echo","enabled":false,"source_version":2,"target_version":1}]}`},
		{name: "复制坏版本", path: "/api/plugin-runtimes/group-maintenance/copy", body: `{"source_group_id":100,"target_group_id":200,"states":[{"plugin_key":"echo","enabled":true,"source_version":0,"target_version":0}]}`},
		{name: "复制缺少源值", path: "/api/plugin-runtimes/group-maintenance/copy", body: `{"source_group_id":100,"target_group_id":200,"states":[{"plugin_key":"echo","source_version":1,"target_version":0}]}`},
		{name: "复制缺少目标版本", path: "/api/plugin-runtimes/group-maintenance/copy", body: `{"source_group_id":100,"target_group_id":200,"states":[{"plugin_key":"echo","enabled":false,"source_version":1}]}`},
		{name: "清理预览未知字段", path: "/api/plugin-runtimes/group-maintenance/stale/preview", body: `{"extra":true}`},
		{name: "清理空状态", path: "/api/plugin-runtimes/group-maintenance/stale/cleanup", body: `{"states":[]}`},
		{name: "清理重复状态", path: "/api/plugin-runtimes/group-maintenance/stale/cleanup", body: `{"states":[{"plugin_key":"echo","group_id":300,"expected_version":1},{"plugin_key":"echo","group_id":300,"expected_version":1}]}`},
		{name: "清理坏版本", path: "/api/plugin-runtimes/group-maintenance/stale/cleanup", body: `{"states":[{"plugin_key":"echo","group_id":300,"expected_version":0}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtimes := &fakeRuntimeStates{}
			server, token := newRuntimeTestServer(t, runtimes)
			recorder := runtimeRequest(t, server, token, http.MethodPost, test.path, test.body, "req-invalid-maintenance")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if len(runtimes.calls) != 0 {
				t.Fatalf("service called: %v", runtimes.calls)
			}
		})
	}

	items := make([]plugin.RuntimeGroupCopyItem, plugin.RuntimeGroupBatchLimit+1)
	for index := range items {
		items[index] = plugin.RuntimeGroupCopyItem{PluginKey: "echo", Enabled: true, SourceVersion: 1}
	}
	payload, err := json.Marshal(map[string]any{"source_group_id": 100, "target_group_id": 200, "states": items})
	if err != nil {
		t.Fatal(err)
	}
	runtimes := &fakeRuntimeStates{}
	server, token := newRuntimeTestServer(t, runtimes)
	recorder := runtimeRequest(t, server, token, http.MethodPost, "/api/plugin-runtimes/group-maintenance/copy", string(payload), "req-too-large")
	if recorder.Code != http.StatusBadRequest || len(runtimes.calls) != 0 {
		t.Fatalf("too large status=%d body=%s calls=%v", recorder.Code, recorder.Body.String(), runtimes.calls)
	}
}

func TestPluginRuntimeRoutesMapDomainErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		code string
	}{
		{name: "插件不存在", err: plugin.ErrRuntimePluginNotFound, want: http.StatusNotFound, code: "plugin_runtime_not_found"},
		{name: "状态行缺失", err: plugin.ErrRuntimeStateNotFound, want: http.StatusNotFound, code: "plugin_runtime_not_found"},
		{name: "版本冲突", err: plugin.ErrRuntimeStateConflict, want: http.StatusConflict, code: "plugin_runtime_conflict"},
		{name: "正在切换", err: plugin.ErrRuntimeTransition, want: http.StatusConflict, code: "plugin_runtime_transition"},
		{name: "需先禁用", err: plugin.ErrRuntimeRecoveryNeeded, want: http.StatusConflict, code: "plugin_runtime_recovery_needed"},
		{name: "群号非法", err: plugin.ErrInvalidRuntimeGroupID, want: http.StatusBadRequest, code: "invalid_plugin_runtime"},
		{name: "源群无开关", err: plugin.ErrRuntimeGroupSourceStateNotFound, want: http.StatusNotFound, code: "plugin_runtime_group_source_state_not_found"},
		{name: "群目录不可用", err: plugin.ErrRuntimeGroupDirectoryUnavailable, want: http.StatusServiceUnavailable, code: "plugin_runtime_group_directory_unavailable"},
		{name: "提交结果未知", err: plugin.ErrRuntimeStateCommitUnknown, want: http.StatusServiceUnavailable, code: "plugin_runtime_commit_unknown"},
		{name: "群已退出", err: plugin.ErrRuntimeGroupNotLive, want: http.StatusConflict, code: "plugin_runtime_group_not_live"},
		{name: "群重新出现", err: plugin.ErrRuntimeGroupStateNotStale, want: http.StatusConflict, code: "plugin_runtime_group_not_stale"},
		{name: "无权限", err: admin.ErrForbidden, want: http.StatusForbidden, code: "forbidden"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtimes := &fakeRuntimeStates{err: test.err}
			server, token := newRuntimeTestServer(t, runtimes)
			recorder := runtimeRequest(t, server, token, http.MethodPatch, "/api/plugin-runtimes/echo", `{"enabled":true,"expected_version":1}`, "req-error")
			if recorder.Code != test.want || !strings.Contains(recorder.Body.String(), test.code) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestPluginRuntimeRoutesRequireAuthentication(t *testing.T) {
	runtimes := &fakeRuntimeStates{}
	server, _ := newRuntimeTestServer(t, runtimes)
	paths := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/plugin-runtimes"},
		{method: http.MethodGet, path: "/api/plugin-runtimes/echo"},
		{method: http.MethodPatch, path: "/api/plugin-runtimes/echo"},
		{method: http.MethodPut, path: "/api/plugin-runtimes/echo/groups/100"},
		{method: http.MethodPost, path: "/api/plugin-runtimes/group-maintenance/copy/preview"},
		{method: http.MethodPost, path: "/api/plugin-runtimes/group-maintenance/copy"},
		{method: http.MethodPost, path: "/api/plugin-runtimes/group-maintenance/stale/preview"},
		{method: http.MethodPost, path: "/api/plugin-runtimes/group-maintenance/stale/cleanup"},
		{method: http.MethodGet, path: "/api/plugin-runtimes/echo/config"},
		{method: http.MethodPut, path: "/api/plugin-runtimes/echo/config"},
	}
	for _, target := range paths {
		request := httptest.NewRequest(target.method, target.path, strings.NewReader(`{"enabled":true,"expected_version":1}`))
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d", target.method, target.path, recorder.Code)
		}
	}
	if len(runtimes.calls) != 0 {
		t.Fatalf("unauthenticated call reached service: %v", runtimes.calls)
	}
}

func TestNewRejectsMissingRuntimeController(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{}}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, &fakePlugins{}, nil, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	if server != nil || err == nil {
		t.Fatalf("New() = %v,%v", server, err)
	}
}

func TestPluginRuntimeConfigRoutes(t *testing.T) {
	runtimes := &fakeRuntimeStates{
		config: plugin.RuntimeConfigView{
			PluginKey: "echo",
			Schema:    plugin.ConfigSchema{Fields: []plugin.ConfigField{{Key: "response_prefix", DisplayName: "回复前缀", Type: plugin.FieldString}}},
			Config:    json.RawMessage(`{"response_prefix":"[bot] "}`),
			Version:   2,
		},
	}
	server, token := newRuntimeTestServer(t, runtimes)

	getRecorder := runtimeRequest(t, server, token, http.MethodGet, "/api/plugin-runtimes/echo/config", "", "req-config")
	body := getRecorder.Body.String()
	if getRecorder.Code != http.StatusOK || !strings.Contains(body, `"response_prefix"`) || !strings.Contains(body, `"version":2`) {
		t.Fatalf("get status=%d body=%s", getRecorder.Code, body)
	}

	putRecorder := runtimeRequest(t, server, token, http.MethodPut, "/api/plugin-runtimes/echo/config", `{"config":{"response_prefix":"[bot] "},"expected_version":2}`, "req-save")
	if putRecorder.Code != http.StatusOK || runtimes.version != 2 || runtimes.key != "echo" {
		t.Fatalf("put status=%d fake=%+v", putRecorder.Code, runtimes)
	}
	// 配置载荷必须原样透传给服务层，由平台按 Schema 规范化。
	if !strings.Contains(string(runtimes.payload), `"response_prefix":"[bot] "`) {
		t.Fatalf("payload = %s", runtimes.payload)
	}
	if runtimes.actor.RequestID != "req-save" || runtimes.actor.Channel != management.ChannelWebUI {
		t.Fatalf("actor = %+v", runtimes.actor)
	}
}

func TestPluginRuntimeConfigRoutesMapDomainErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		code string
	}{
		{name: "未声明配置", err: plugin.ErrRuntimeConfigNotSupported, want: http.StatusNotFound, code: "plugin_runtime_config_not_supported"},
		{name: "配置行缺失", err: plugin.ErrRuntimeConfigNotFound, want: http.StatusNotFound, code: "plugin_runtime_config_not_supported"},
		{name: "配置无效", err: plugin.ErrRuntimeConfigInvalid, want: http.StatusBadRequest, code: "invalid_plugin_runtime_config"},
		{name: "版本冲突", err: plugin.ErrRuntimeStateConflict, want: http.StatusConflict, code: "plugin_runtime_conflict"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtimes := &fakeRuntimeStates{err: test.err}
			server, token := newRuntimeTestServer(t, runtimes)
			recorder := runtimeRequest(t, server, token, http.MethodPut, "/api/plugin-runtimes/echo/config", `{"config":{},"expected_version":1}`, "req-error")
			if recorder.Code != test.want || !strings.Contains(recorder.Body.String(), test.code) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
