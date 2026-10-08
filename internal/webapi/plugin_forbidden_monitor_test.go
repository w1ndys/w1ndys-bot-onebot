// 📌 影响范围：执行 Argon2id 哈希；使用内存 HTTP 测试请求，不访问数据库或网络。
package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/w1ndys-bot/internal/admin"
	"github.com/w1ndys/w1ndys-bot/internal/management"
	forbiddenmonitor "github.com/w1ndys/w1ndys-bot/plugins/forbidden_message_monitor"
)

type fakeForbiddenMonitor struct {
	page              forbiddenmonitor.RecordPage
	record            forbiddenmonitor.Record
	term              forbiddenmonitor.Term
	err               error
	actor             management.Actor
	id                int64
	version           int64
	status            string
	text              string
	violationPage     int
	violationPageSize int
	termPage          int
	termPageSize      int
	termInput         forbiddenmonitor.TermInput
	calls             []string
}

func (f *fakeForbiddenMonitor) ListViolations(_ context.Context, actor management.Actor, page, pageSize int) (forbiddenmonitor.RecordPage, error) {
	f.actor, f.violationPage, f.violationPageSize = actor, page, pageSize
	f.calls = append(f.calls, "list-violations")
	return f.page, f.err
}

func (f *fakeForbiddenMonitor) ReviewViolation(_ context.Context, actor management.Actor, id, version int64, status string) (forbiddenmonitor.Record, error) {
	f.actor, f.id, f.version, f.status = actor, id, version, status
	f.calls = append(f.calls, "review")
	return f.record, f.err
}

func (f *fakeForbiddenMonitor) RunTextTrial(_ context.Context, actor management.Actor, text string) (forbiddenmonitor.Record, error) {
	f.actor, f.text, f.calls = actor, text, append(f.calls, "trial")
	return f.record, f.err
}

func (f *fakeForbiddenMonitor) ListTerms(_ context.Context, actor management.Actor, page, pageSize int) ([]forbiddenmonitor.Term, int64, error) {
	f.actor, f.termPage, f.termPageSize = actor, page, pageSize
	f.calls = append(f.calls, "list-terms")
	return []forbiddenmonitor.Term{f.term}, 1, f.err
}

func (f *fakeForbiddenMonitor) CreateTerm(_ context.Context, actor management.Actor, input forbiddenmonitor.TermInput) (forbiddenmonitor.Term, error) {
	f.actor, f.termInput, f.calls = actor, input, append(f.calls, "create-term")
	return f.term, f.err
}

func (f *fakeForbiddenMonitor) UpdateTerm(_ context.Context, actor management.Actor, id, version int64, input forbiddenmonitor.TermInput) (forbiddenmonitor.Term, error) {
	f.actor, f.id, f.version, f.termInput = actor, id, version, input
	f.calls = append(f.calls, "update-term")
	return f.term, f.err
}

func (f *fakeForbiddenMonitor) DeleteTerm(_ context.Context, actor management.Actor, id, version int64) error {
	f.actor, f.id, f.version = actor, id, version
	f.calls = append(f.calls, "delete-term")
	return f.err
}

func newMonitorTestServer(t *testing.T, monitor *fakeForbiddenMonitor) (*Server, string) {
	t.Helper()
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Enabled: true}}}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, &fakePlugins{}, &fakeRuntimeStates{}, &fakeKeywordReply{}, monitor)
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

func TestForbiddenMonitorRoutesReadAndWrite(t *testing.T) {
	monitor := &fakeForbiddenMonitor{
		page:   forbiddenmonitor.RecordPage{Items: []forbiddenmonitor.Record{{ID: 1, Version: 1, Data: json.RawMessage(`{"msg_content":"广告"}`)}}, Page: 2, PageSize: 10, Total: 11},
		record: forbiddenmonitor.Record{ID: 1, Version: 2, Data: json.RawMessage(`{"decision":"违规"}`)},
		term:   forbiddenmonitor.Term{ID: 5, Text: "加微信", Weight: 25, Version: 2},
	}
	server, token := newMonitorTestServer(t, monitor)

	if recorder := runtimeRequest(t, server, token, http.MethodGet, "/api/plugins/forbidden_message_monitor/violations?page=2&page_size=10", "", "req-1"); recorder.Code != http.StatusOK || monitor.violationPage != 2 || monitor.violationPageSize != 10 {
		t.Fatalf("violations status=%d page=%d pageSize=%d", recorder.Code, monitor.violationPage, monitor.violationPageSize)
	}
	reviewRecorder := runtimeRequest(t, server, token, http.MethodPost, "/api/plugins/forbidden_message_monitor/violations/1/review", `{"status":"确认","expected_version":1}`, "req-2")
	if reviewRecorder.Code != http.StatusOK || monitor.id != 1 || monitor.version != 1 || monitor.status != "确认" {
		t.Fatalf("review status=%d fake=%+v", reviewRecorder.Code, monitor)
	}

	trialRecorder := runtimeRequest(t, server, token, http.MethodPost, "/api/plugins/forbidden_message_monitor/text-trials", `{"text":"加微信看片"}`, "req-3")
	if trialRecorder.Code != http.StatusOK || monitor.text != "加微信看片" {
		t.Fatalf("trial status=%d text=%q", trialRecorder.Code, monitor.text)
	}

	listTermsRecorder := runtimeRequest(t, server, token, http.MethodGet, "/api/plugins/forbidden_message_monitor/terms?page=3&page_size=15", "", "req-4")
	if listTermsRecorder.Code != http.StatusOK || monitor.termPage != 3 || monitor.termPageSize != 15 || !strings.Contains(listTermsRecorder.Body.String(), `"total":1`) {
		t.Fatalf("terms status=%d page=%d pageSize=%d body=%s", listTermsRecorder.Code, monitor.termPage, monitor.termPageSize, listTermsRecorder.Body.String())
	}

	termRecorder := runtimeRequest(t, server, token, http.MethodPost, "/api/plugins/forbidden_message_monitor/terms", `{"text":"加微信","weight":25}`, "req-5")
	if termRecorder.Code != http.StatusOK || monitor.termInput.Text != "加微信" || monitor.termInput.Weight != 25 {
		t.Fatalf("term status=%d input=%+v", termRecorder.Code, monitor.termInput)
	}

	updateRecorder := runtimeRequest(t, server, token, http.MethodPut, "/api/plugins/forbidden_message_monitor/terms/5", `{"text":"加好友","weight":30,"expected_version":1}`, "req-6")
	if updateRecorder.Code != http.StatusOK || monitor.id != 5 || monitor.version != 1 || monitor.termInput.Text != "加好友" || monitor.termInput.Weight != 30 {
		t.Fatalf("update status=%d fake=%+v", updateRecorder.Code, monitor)
	}

	deleteRecorder := runtimeRequest(t, server, token, http.MethodDelete, "/api/plugins/forbidden_message_monitor/terms/5", `{"expected_version":2}`, "req-7")
	if deleteRecorder.Code != http.StatusOK || monitor.id != 5 || monitor.version != 2 {
		t.Fatalf("delete status=%d fake=%+v", deleteRecorder.Code, monitor)
	}
	// 管理通道与请求 ID 必须由平台注入。
	if monitor.actor.Channel != management.ChannelWebUI || monitor.actor.RequestID != "req-7" {
		t.Fatalf("actor = %+v", monitor.actor)
	}
}

func TestForbiddenMonitorRoutesRejectInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "复核缺少版本", method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/violations/1/review", body: `{"status":"确认"}`},
		{name: "复核缺少结论", method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/violations/1/review", body: `{"expected_version":1}`},
		{name: "复核 ID 非法", method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/violations/0/review", body: `{"status":"确认","expected_version":1}`},
		{name: "试判空文本", method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/text-trials", body: `{"text":""}`},
		{name: "新增词条携带旧分类", method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/terms", body: `{"kind":"risk","text":"x","weight":1}`},
		{name: "更新词条携带旧分类", method: http.MethodPut, path: "/api/plugins/forbidden_message_monitor/terms/1", body: `{"kind":"risk","text":"x","weight":1,"expected_version":1}`},
		{name: "新增词条携带版本", method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/terms", body: `{"text":"x","weight":1,"expected_version":1}`},
		{name: "新增词条携带零版本", method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/terms", body: `{"text":"x","weight":1,"expected_version":0}`},
		{name: "更新词条缺少版本", method: http.MethodPut, path: "/api/plugins/forbidden_message_monitor/terms/1", body: `{"text":"x","weight":1}`},
		{name: "删除词条缺少版本", method: http.MethodDelete, path: "/api/plugins/forbidden_message_monitor/terms/1", body: `{}`},
		{name: "未知字段", method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/text-trials", body: `{"text":"x","extra":1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			monitor := &fakeForbiddenMonitor{}
			server, token := newMonitorTestServer(t, monitor)
			recorder := runtimeRequest(t, server, token, test.method, test.path, test.body, "req-invalid")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			// 输入校验失败不得触达服务层。
			if len(monitor.calls) != 0 {
				t.Fatalf("service called: %v", monitor.calls)
			}
		})
	}
}

func TestForbiddenMonitorRoutesMapDomainErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		code string
	}{
		{name: "记录不存在", err: forbiddenmonitor.ErrLexiconNotFound, want: http.StatusNotFound, code: "forbidden_monitor_not_found"},
		{name: "词条冲突", err: forbiddenmonitor.ErrLexiconConflict, want: http.StatusConflict, code: "forbidden_monitor_conflict"},
		{name: "输入无效", err: forbiddenmonitor.ErrInvalidInput, want: http.StatusBadRequest, code: "invalid_forbidden_monitor"},
		{name: "词库约束", err: forbiddenmonitor.ErrInvalidLexicon, want: http.StatusBadRequest, code: "invalid_forbidden_monitor"},
		{name: "无权限", err: admin.ErrForbidden, want: http.StatusForbidden, code: "forbidden"},
		{name: "运行门禁关闭", err: forbiddenmonitor.ErrRuntimeUnavailable, want: http.StatusConflict, code: "forbidden_monitor_runtime_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			monitor := &fakeForbiddenMonitor{err: test.err}
			server, token := newMonitorTestServer(t, monitor)
			recorder := runtimeRequest(t, server, token, http.MethodPost, "/api/plugins/forbidden_message_monitor/terms", `{"text":"x","weight":1}`, "req-error")
			if recorder.Code != test.want || !strings.Contains(recorder.Body.String(), test.code) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestForbiddenMonitorRoutesMapOptimisticLockConflict(t *testing.T) {
	monitor := &fakeForbiddenMonitor{err: forbiddenmonitor.ErrLexiconConflict}
	server, token := newMonitorTestServer(t, monitor)
	recorder := runtimeRequest(t, server, token, http.MethodPut, "/api/plugins/forbidden_message_monitor/terms/5", `{"text":"加好友","weight":30,"expected_version":7}`, "req-conflict")
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"code":"forbidden_monitor_conflict"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if monitor.id != 5 || monitor.version != 7 || len(monitor.calls) != 1 || monitor.calls[0] != "update-term" {
		t.Fatalf("fake=%+v", monitor)
	}
}

func TestForbiddenMonitorRoutesRejectInvalidPagination(t *testing.T) {
	for _, target := range []string{
		"/api/plugins/forbidden_message_monitor/violations?page=0",
		"/api/plugins/forbidden_message_monitor/violations?kind=risk",
		"/api/plugins/forbidden_message_monitor/violations?page=1&page=2",
		"/api/plugins/forbidden_message_monitor/terms?page=abc",
		"/api/plugins/forbidden_message_monitor/terms?kind=hard",
		"/api/plugins/forbidden_message_monitor/terms?page=",
		"/api/plugins/forbidden_message_monitor/terms?page_size=201",
	} {
		monitor := &fakeForbiddenMonitor{}
		server, token := newMonitorTestServer(t, monitor)
		recorder := runtimeRequest(t, server, token, http.MethodGet, target, "", "req-page")
		if recorder.Code != http.StatusBadRequest || len(monitor.calls) != 0 {
			t.Fatalf("%s status=%d calls=%v", target, recorder.Code, monitor.calls)
		}
	}
}

func TestForbiddenMonitorRoutesRequireAuthentication(t *testing.T) {
	monitor := &fakeForbiddenMonitor{}
	server, _ := newMonitorTestServer(t, monitor)
	targets := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/plugins/forbidden_message_monitor/violations"},
		{method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/text-trials"},
		{method: http.MethodGet, path: "/api/plugins/forbidden_message_monitor/terms"},
		{method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/terms"},
		{method: http.MethodPut, path: "/api/plugins/forbidden_message_monitor/terms/1"},
		{method: http.MethodDelete, path: "/api/plugins/forbidden_message_monitor/terms/1"},
	}
	for _, target := range targets {
		recorder := runtimeRequest(t, server, "", target.method, target.path, `{"expected_version":1}`, "req-anon")
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d", target.method, target.path, recorder.Code)
		}
	}
	if len(monitor.calls) != 0 {
		t.Fatalf("unauthenticated call reached service: %v", monitor.calls)
	}
}

func TestNewRejectsMissingForbiddenMonitorController(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{}}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, &fakePlugins{}, &fakeRuntimeStates{}, &fakeKeywordReply{}, nil)
	if server != nil || err == nil {
		t.Fatalf("New() = %v,%v", server, err)
	}
}
