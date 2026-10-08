// 📌 影响范围：执行 Argon2id 哈希；使用内存 HTTP 测试请求，不访问数据库或网络。
package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/w1ndys-bot/internal/admin"
	"github.com/w1ndys/w1ndys-bot/internal/management"
)

type fakeAdmins struct {
	accounts map[string]admin.SystemAdmin
}

type fakePlugins struct {
	actor        management.Actor
	err          error
	settings     []management.SettingState
	setting      management.SettingState
	settingKey   string
	settingValue json.RawMessage
	auditPage    management.AuditPage
	audit        management.AuditState
	auditQuery   management.AuditQuery
	auditID      int64
}

func (f *fakePlugins) ListAuditLogs(_ context.Context, actor management.Actor, query management.AuditQuery) (management.AuditPage, error) {
	f.actor, f.auditQuery = actor, query

	// >>> 数据演变示例
	// 1. page1+actor100 -> 记录 -> auditPage,nil。
	// 2. err=boom -> 返回预设错误。
	return f.auditPage, f.err
}

// GetAuditLog 返回测试审计详情并记录 ID。
// @param ctx：未使用的请求上下文；actor：WebUI 操作者；id：审计ID。
// @returns 预设审计记录或错误。
// ⚠️副作用说明：记录 Actor 和审计ID。
func (f *fakePlugins) GetAuditLog(_ context.Context, actor management.Actor, id int64) (management.AuditState, error) {
	f.actor, f.auditID = actor, id

	// >>> 数据演变示例
	// 1. id8+actor100 -> 记录 -> audit,nil。
	// 2. id404+ErrAuditNotFound -> 记录 -> error。
	return f.audit, f.err
}

// ListSettings 返回测试设置列表并记录 Actor。
// @param ctx：未使用的请求上下文；actor：WebUI 操作者。
// @returns 预设设置列表或错误。
// ⚠️副作用说明：记录最近一次 Actor。
func (f *fakePlugins) ListSettings(_ context.Context, actor management.Actor) ([]management.SettingState, error) {
	f.actor = actor

	// >>> 数据演变示例
	// 1. settings=[prefix]+actor100 -> 记录actor -> 返回列表。
	// 2. err=boom -> 返回预设错误。
	return f.settings, f.err
}

// SetSetting 记录系统设置保存操作。
// @param ctx：未使用的上下文；actor：操作者；key：设置键；value：JSON值。
// @returns 预设设置状态或错误。
// ⚠️副作用说明：记录 Actor、设置键和 JSON 值副本。
func (f *fakePlugins) SetSetting(_ context.Context, actor management.Actor, key string, value json.RawMessage) (management.SettingState, error) {
	f.actor, f.settingKey = actor, key
	f.settingValue = append(json.RawMessage(nil), value...)

	// >>> 数据演变示例
	// 1. prefix+"!" -> 记录副本 -> setting,nil。
	// 2. page_size+500+ErrInvalidSetting -> 记录 -> error。
	return f.setting, f.err
}

// DeleteSetting 记录系统设置覆盖删除操作。
// @param ctx：未使用的上下文；actor：操作者；key：设置键。
// @returns 预设错误。
// ⚠️副作用说明：记录 Actor 和设置键。
func (f *fakePlugins) DeleteSetting(_ context.Context, actor management.Actor, key string) error {
	f.actor, f.settingKey = actor, key

	// >>> 数据演变示例
	// 1. prefix -> 记录 -> nil。
	// 2. 未覆盖prefix -> 记录 -> ErrSettingNotFound。
	return f.err
}

// Resolve 返回测试管理员快照。
func (f *fakeAdmins) Resolve(userID string) (admin.SystemAdmin, bool) {
	account, exists := f.accounts[userID]

	// >>> 数据演变示例
	// 1. map{100}+100 -> account,true。
	// 2. map{100}+200 -> 零值,false。
	return account, exists
}

// TestLoginAndMe 验证有效管理员登录后可使用 JWT 查询当前身份。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：执行 Argon2id 哈希并创建内存 HTTP 请求。
func TestLoginAndMe(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Nickname: "root", Enabled: true}}}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, &fakePlugins{}, &fakeRuntimeStates{}, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	// [决策理由] 合法配置必须成功构造服务。
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	server.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"qq":"100","password":"correct-horse-battery-staple"}`))
	loginRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(loginRecorder, login)
	// [决策理由] 正确身份必须得到令牌而不是认证错误。
	if loginRecorder.Code != http.StatusOK {
		t.Fatalf("login status = %d body=%s", loginRecorder.Code, loginRecorder.Body.String())
	}
	var response struct {
		Code string `json:"code"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	// [决策理由] 登录响应必须符合统一JSON结构并包含Token。
	if err := json.Unmarshal(loginRecorder.Body.Bytes(), &response); err != nil || response.Code != "ok" || response.Data.Token == "" {
		t.Fatalf("login response = %#v error=%v", response, err)
	}
	me := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	me.Header.Set("Authorization", "Bearer "+response.Data.Token)
	meRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(meRecorder, me)
	// [决策理由] 有效Token必须能读取当前管理员且带安全头。
	if meRecorder.Code != http.StatusOK || !strings.Contains(meRecorder.Body.String(), `"UserID":"100"`) || meRecorder.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("me status=%d body=%s headers=%v", meRecorder.Code, meRecorder.Body.String(), meRecorder.Header())
	}

	// >>> 数据演变示例
	// 1. 100+正确密码 -> JWT -> /me 200及管理员100。
	// 2. 响应缺Token -> JSON断言失败 -> 测试失败。
}

// TestLoginRejectsInvalidCredentials 验证错误密码和非管理员使用相同失败响应。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：执行多次 Argon2id 校验并创建内存 HTTP 请求。
func TestLoginRejectsInvalidCredentials(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Enabled: true}}}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, &fakePlugins{}, &fakeRuntimeStates{}, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	// [决策理由] 测试前置服务必须构造成功。
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	bodies := []string{
		`{"qq":"100","password":"wrong-password-value"}`,
		`{"qq":"200","password":"correct-horse-battery-staple"}`,
	}
	var firstBody string
	for index, body := range bodies {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body)))
		// [决策理由] 账号或密码任一无效都必须返回401。
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("case %d status=%d body=%s", index, recorder.Code, recorder.Body.String())
		}
		// [决策理由] 两类失败内容必须一致，避免枚举管理员QQ。
		if index == 0 {
			firstBody = recorder.Body.String()
		} else if recorder.Body.String() != firstBody {
			t.Fatalf("credential responses differ: %q != %q", recorder.Body.String(), firstBody)
		}
	}

	// >>> 数据演变示例
	// 1. 管理员+错误密码 -> 401 invalid_credentials。
	// 2. 非管理员+正确密码 -> 相同401响应。
}

// TestMeRejectsExpiredAndRevokedToken 验证过期 Token 与管理员热禁用均立即失效。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：执行 Argon2id 哈希并修改测试内存管理员映射。
func TestMeRejectsExpiredAndRevokedToken(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Enabled: true}}}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, &fakePlugins{}, &fakeRuntimeStates{}, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	// [决策理由] 测试前置服务必须构造成功。
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	current := time.Unix(1_700_000_000, 0)
	server.now = func() time.Time { return current }
	token, err := server.sign("100")
	// [决策理由] 合法管理员必须可签发测试Token。
	if err != nil {
		t.Fatalf("sign() error = %v", err)
	}
	delete(admins.accounts, "100")
	revoked := requestMe(server, token)
	// [决策理由] 管理员从快照移除后旧Token必须被拒绝。
	if revoked.Code != http.StatusUnauthorized {
		t.Fatalf("revoked status = %d", revoked.Code)
	}
	admins.accounts["100"] = admin.SystemAdmin{UserID: "100", Enabled: true}
	current = current.Add(tokenLifetime + time.Second)
	expired := requestMe(server, token)
	// [决策理由] 超过exp后即使管理员恢复启用也必须重新登录。
	if expired.Code != http.StatusUnauthorized {
		t.Fatalf("expired status = %d", expired.Code)
	}

	// >>> 数据演变示例
	// 1. 有效Token+管理员删除 -> 快照复核失败 -> 401。
	// 2. 有效管理员+Token超过12小时 -> exp校验失败 -> 401。
}

// requestMe 使用指定 Token 调用当前身份接口。
// @param server：测试服务；token：Bearer Token。
// @returns 已完成的响应记录器。
// ⚠️副作用说明：创建并执行内存 HTTP 请求。
func requestMe(server *Server, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	// >>> 数据演变示例
	// 1. 有效Token -> Handler -> 200记录器。
	// 2. 过期Token -> Handler -> 401记录器。
	return recorder
}

// TestPluginRoutesListAndPatch 验证插件管理路由传递审计身份并返回稳定 JSON。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：执行 Argon2id 哈希并创建内存 HTTP 请求。
func TestSettingRoutesListSetAndDelete(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Enabled: true}}}
	controller := &fakePlugins{setting: management.SettingState{Key: "command_prefix", Value: json.RawMessage(`"!"`), Description: "机器人命令前缀", Overridden: true}}
	controller.settings = []management.SettingState{controller.setting, {Key: "default_page_size", Value: json.RawMessage(`20`), Description: "管理列表默认分页大小"}}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, controller, &fakeRuntimeStates{}, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	// [决策理由] 完整控制器必须成功构造设置 API 服务。
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	server.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	token, err := server.sign("100")
	// [决策理由] 设置接口均要求有效管理员 Token。
	if err != nil {
		t.Fatalf("sign() error = %v", err)
	}
	list := requestAPI(server, token, http.MethodGet, "/api/settings", "", "req-list-setting")
	// [决策理由] 列表必须保留 JSON 值类型和覆盖状态。
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"value":"!"`) || !strings.Contains(list.Body.String(), `"overridden":true`) {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	set := requestAPI(server, token, http.MethodPut, "/api/settings/command_prefix", `{"value":"!"}`, "req-set-setting")
	// [决策理由] 设置键、原始 JSON 和请求ID必须完整传入管理服务。
	if set.Code != http.StatusOK || controller.settingKey != "command_prefix" || string(controller.settingValue) != `"!"` || controller.actor.RequestID != "req-set-setting" {
		t.Fatalf("set status=%d key=%s value=%s actor=%+v", set.Code, controller.settingKey, controller.settingValue, controller.actor)
	}
	deleted := requestAPI(server, token, http.MethodDelete, "/api/settings/command_prefix", "", "req-delete-setting")
	// [决策理由] 删除覆盖应传递设置键并返回统一空数据成功响应。
	if deleted.Code != http.StatusOK || controller.settingKey != "command_prefix" || !strings.Contains(deleted.Body.String(), `"data":null`) {
		t.Fatalf("delete status=%d key=%s body=%s", deleted.Code, controller.settingKey, deleted.Body.String())
	}

	// >>> 数据演变示例
	// 1. PUT prefix="!" -> UPSERT+审计+热刷新 -> 200 overridden=true。
	// 2. DELETE prefix -> 删除覆盖+回退默认/ -> 200 data:null。
}

// TestSettingRoutesMapInvalidAndUnknown 验证设置值、未知键及无覆盖错误映射。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：执行 Argon2id 哈希并创建内存 HTTP 请求。
func TestSettingRoutesMapInvalidAndUnknown(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Enabled: true}}}
	controller := &fakePlugins{err: admin.ErrInvalidSetting}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, controller, &fakeRuntimeStates{}, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	// [决策理由] 错误映射测试需要完整服务依赖。
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	server.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	token, err := server.sign("100")
	// [决策理由] 必须通过认证后才能到达设置领域错误映射。
	if err != nil {
		t.Fatalf("sign() error = %v", err)
	}
	invalid := requestAPI(server, token, http.MethodPut, "/api/settings/default_page_size", `{"value":500}`, "")
	// [决策理由] 超范围值应映射为400稳定业务码。
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"invalid_setting"`) {
		t.Fatalf("invalid status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	controller.err = admin.ErrUnknownSetting
	unknown := requestAPI(server, token, http.MethodPut, "/api/settings/db_password", `{"value":"secret"}`, "")
	// [决策理由] 未注册敏感键应映射为404且不成为动态设置。
	if unknown.Code != http.StatusNotFound || !strings.Contains(unknown.Body.String(), `"code":"unknown_setting"`) {
		t.Fatalf("unknown status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	controller.err = admin.ErrSettingNotFound
	missingOverride := requestAPI(server, token, http.MethodDelete, "/api/settings/command_prefix", "", "")
	// [决策理由] 已使用默认值时再次删除应明确返回无覆盖状态。
	if missingOverride.Code != http.StatusNotFound || !strings.Contains(missingOverride.Body.String(), `"code":"setting_override_not_found"`) {
		t.Fatalf("missing override status=%d body=%s", missingOverride.Code, missingOverride.Body.String())
	}

	// >>> 数据演变示例
	// 1. page_size=500 -> ErrInvalidSetting -> 400 invalid_setting。
	// 2. db_password -> ErrUnknownSetting -> 404 unknown_setting。
}

// TestAuditRoutesListAndDetail 验证审计分页筛选、详情快照和管理员请求上下文。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：执行 Argon2id 哈希并创建内存 HTTP 请求。
func TestAuditRoutesListAndDetail(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Enabled: true}}}
	createdAt := time.Date(2026, 7, 13, 10, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	record := management.AuditState{ID: 8, ActorID: "100", ActorRole: "super_admin", Channel: "webui", Action: "setting.set", TargetType: "system_setting", TargetID: "command_prefix", BeforeJSON: json.RawMessage(`{"Value":"/","token":"old-secret"}`), AfterJSON: json.RawMessage(`{"Value":"!","nested":{"password":"new-secret"}}`), Success: true, RequestID: "req-write", CreatedAt: createdAt}
	controller := &fakePlugins{audit: record, auditPage: management.AuditPage{Items: []management.AuditState{record}, Page: 2, PageSize: 10, Total: 21}}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, controller, &fakeRuntimeStates{}, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	// [决策理由] 完整控制器必须成功构造审计 API 服务。
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	server.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	token, err := server.sign("100")
	// [决策理由] 审计接口只允许已认证最高管理员访问。
	if err != nil {
		t.Fatalf("sign() error = %v", err)
	}
	list := requestAPI(server, token, http.MethodGet, "/api/audit-logs?page=2&page_size=10&actor_id=100&action=setting.set&start_time=2026-07-13T00:00:00%2B08:00", "", "req-list-audit")
	// [决策理由] 分页、筛选和审计 Actor 必须完整传入服务，响应保留总数。
	if list.Code != http.StatusOK || controller.auditQuery.Page != 2 || controller.auditQuery.PageSize != 10 || controller.auditQuery.ActorID != "100" || controller.auditQuery.StartTime == nil || controller.actor.RequestID != "req-list-audit" || !strings.Contains(list.Body.String(), `"total":21`) || strings.Contains(list.Body.String(), `"before"`) {
		t.Fatalf("list status=%d query=%+v actor=%+v body=%s", list.Code, controller.auditQuery, controller.actor, list.Body.String())
	}
	detail := requestAPI(server, token, http.MethodGet, "/api/audit-logs/8", "", "req-detail-audit")
	// [决策理由] 详情接口必须传递ID、保留非敏感值并在服务端脱敏凭据字段。
	if detail.Code != http.StatusOK || controller.auditID != 8 || !strings.Contains(detail.Body.String(), `"Value":"/"`) || !strings.Contains(detail.Body.String(), `"token":"[已脱敏]"`) || !strings.Contains(detail.Body.String(), `"password":"[已脱敏]"`) || strings.Contains(detail.Body.String(), "old-secret") || strings.Contains(detail.Body.String(), "new-secret") || !strings.Contains(detail.Body.String(), `"created_at":"2026-07-13T02:00:00Z"`) {
		t.Fatalf("detail status=%d id=%d body=%s", detail.Code, controller.auditID, detail.Body.String())
	}

	// >>> 数据演变示例
	// 1. GET page2+actor100 -> AuditQuery -> 200 items,total21。
	// 2. GET id8 -> 完整before/after JSON -> 200详情。
}

// TestRedactAuditJSON 验证常见敏感键、嵌套数组和异常JSON均安全闭合。
// @param t：Go测试上下文。
// @returns 无。
// ⚠️副作用说明：无。
func TestRedactAuditJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "common keys", raw: `{"API_Key":"a","authorization":"b","cookie":"c","database_dsn":"d"}`, want: `{"API_Key":"[已脱敏]","authorization":"[已脱敏]","cookie":"[已脱敏]","database_dsn":"[已脱敏]"}`},
		{name: "nested array", raw: `[{"Private-Key":"a"},{"session_id":"b"},{"enabled":true}]`, want: `[{"Private-Key":"[已脱敏]"},{"session_id":"[已脱敏]"},{"enabled":true}]`},
		{name: "invalid json", raw: `token=plain-secret`, want: `null`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := string(redactAuditJSON(json.RawMessage(test.raw)))
			// [决策理由] 脱敏输出必须结构和值完全匹配安全预期。
			if got != test.want {
				t.Fatalf("redactAuditJSON() = %s, want %s", got, test.want)
			}

			// >>> 数据演变示例
			// 1. API_Key输入 -> 规范化命中 -> [已脱敏]。
			// 2. 非法JSON -> 解析失败 -> null。
		})
	}

	// >>> 数据演变示例
	// 1. 三组用例依次执行 -> 全部匹配 -> 测试通过。
	// 2. 任一秘密未替换 -> 字符串不匹配 -> 测试失败。
}

// TestAuditRoutesRejectInvalidQueryAndMapNotFound 验证时间格式、ID和未找到错误映射。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：执行 Argon2id 哈希并创建内存 HTTP 请求。
func TestAuditRoutesRejectInvalidQueryAndMapNotFound(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Enabled: true}}}
	controller := &fakePlugins{}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, controller, &fakeRuntimeStates{}, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	// [决策理由] 错误路径测试需要完整服务依赖。
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	server.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	token, err := server.sign("100")
	// [决策理由] 必须通过认证后才能验证查询参数和领域错误。
	if err != nil {
		t.Fatalf("sign() error = %v", err)
	}
	invalidTime := requestAPI(server, token, http.MethodGet, "/api/audit-logs?start_time=bad", "", "")
	// [决策理由] 非 RFC3339 时间应在调用控制器前返回400。
	if invalidTime.Code != http.StatusBadRequest || !strings.Contains(invalidTime.Body.String(), `"code":"invalid_audit_query"`) || controller.auditQuery.Page != 0 {
		t.Fatalf("invalid time status=%d body=%s query=%+v", invalidTime.Code, invalidTime.Body.String(), controller.auditQuery)
	}
	invalidID := requestAPI(server, token, http.MethodGet, "/api/audit-logs/abc", "", "")
	// [决策理由] 非数字审计 ID 应返回400且不调用详情服务。
	if invalidID.Code != http.StatusBadRequest || controller.auditID != 0 {
		t.Fatalf("invalid id status=%d auditID=%d", invalidID.Code, controller.auditID)
	}
	controller.err = admin.ErrAuditNotFound
	notFound := requestAPI(server, token, http.MethodGet, "/api/audit-logs/404", "", "")
	// [决策理由] 不存在审计记录应映射为404稳定业务码。
	if notFound.Code != http.StatusNotFound || !strings.Contains(notFound.Body.String(), `"code":"audit_not_found"`) {
		t.Fatalf("not found status=%d body=%s", notFound.Code, notFound.Body.String())
	}

	// >>> 数据演变示例
	// 1. start_time=bad -> 400 invalid_audit_query零控制器调用。
	// 2. id404+ErrAuditNotFound -> 404 audit_not_found。
}

// TestLoginRateLimitRejectsSixthAttempt 验证高成本密码校验具有固定窗口限流。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：执行多次 Argon2id 校验并创建内存 HTTP 请求。
func TestLoginRateLimitRejectsSixthAttempt(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Enabled: true}}}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, &fakePlugins{}, &fakeRuntimeStates{}, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	// [决策理由] 限流测试需要合法服务实例。
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	server.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	for index := 0; index < loginAttemptLimit; index++ {
		response := requestLogin(server, "100", "wrong-password-value")
		// [决策理由] 窗口内前五次应完成密码验证并返回普通认证失败。
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status=%d", index+1, response.Code)
		}
	}
	limited := requestLogin(server, "100", "wrong-password-value")
	// [决策理由] 第六次必须在 Argon2 前返回429稳定业务码。
	if limited.Code != http.StatusTooManyRequests || !strings.Contains(limited.Body.String(), `"code":"login_rate_limited"`) {
		t.Fatalf("limited status=%d body=%s", limited.Code, limited.Body.String())
	}

	// >>> 数据演变示例
	// 1. 同IP前5次 -> 401 invalid_credentials。
	// 2. 第6次 -> 429 login_rate_limited。
}

// TestLoginRateLimitCannotBypassWithDifferentQQ 验证轮换伪造 QQ 仍共享 IP 限流窗口。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：执行多次 Argon2id 校验并创建内存 HTTP 请求。
func TestLoginRateLimitCannotBypassWithDifferentQQ(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Enabled: true}}}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, &fakePlugins{}, &fakeRuntimeStates{}, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	// [决策理由] 限流绕过测试需要合法服务实例。
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	server.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	for index := 0; index < loginAttemptLimit; index++ {
		requestLogin(server, strconv.Itoa(200+index), "wrong-password-value")
	}
	limited := requestLogin(server, "999", "wrong-password-value")
	// [决策理由] 同IP更换QQ后第六次仍必须被429拒绝。
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("rotated QQ status=%d body=%s", limited.Code, limited.Body.String())
	}

	// >>> 数据演变示例
	// 1. 同IP依次QQ200..204 -> 共用计数5。
	// 2. 同IP切换QQ999 -> 第6次 -> 429。
}

// TestLoginAttemptMapHasBoundedCleanup 验证登录限流表会淘汰过期记录并保持容量上限。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：直接填充并清理测试服务的内存限流表。
func TestLoginAttemptMapHasBoundedCleanup(t *testing.T) {
	server := &Server{loginAttempts: make(map[string]loginAttempt), loginSlots: make(chan struct{}, 2), now: time.Now}
	now := time.Unix(1_700_000_000, 0)
	for index := 0; index < loginAttemptCapacity; index++ {
		server.loginAttempts[strconv.Itoa(index)] = loginAttempt{Count: 1, WindowStart: now.Add(-2 * loginWindow)}
	}
	server.loginMu.Lock()
	server.cleanupLoginAttemptsLocked(now)
	server.loginMu.Unlock()
	// [决策理由] 所有过期窗口必须被清理，释放固定容量。
	if len(server.loginAttempts) != 0 {
		t.Fatalf("loginAttempts length=%d, want 0", len(server.loginAttempts))
	}

	// >>> 数据演变示例
	// 1. 4096条过期记录 -> cleanup -> 0条。
	// 2. 新来源随后可创建窗口且map不超过4096。
}

// TestStrictJSONAndRequestIDSanitization 验证尾随 JSON 被拒绝且非法请求ID被替换。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：创建并执行内存 HTTP 请求。
func TestStrictJSONAndRequestIDSanitization(t *testing.T) {
	admins := &fakeAdmins{accounts: map[string]admin.SystemAdmin{"100": {UserID: "100", Enabled: true}}}
	controller := &fakePlugins{}
	server, err := New("correct-horse-battery-staple", strings.Repeat("s", 32), admins, controller, &fakeRuntimeStates{}, &fakeKeywordReply{}, &fakeForbiddenMonitor{})
	// [决策理由] 请求安全测试需要完整服务依赖。
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	server.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	token, err := server.sign("100")
	// [决策理由] 需要有效 Token 到达管理请求解码流程。
	if err != nil {
		t.Fatalf("sign() error = %v", err)
	}
	trailing := requestAPI(server, token, http.MethodPut, "/api/settings/command_prefix", `{"value":"!"}{}`, "")
	// [决策理由] 多个 JSON 值必须返回400且不调用控制器。
	if trailing.Code != http.StatusBadRequest || controller.settingKey != "" {
		t.Fatalf("trailing status=%d settingKey=%q", trailing.Code, controller.settingKey)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Request-ID", strings.Repeat("x", 129))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	// [决策理由] 超长客户端ID必须替换为服务端24位十六进制ID再进入Actor。
	if recorder.Code != http.StatusOK || len(controller.actor.RequestID) != 24 || controller.actor.RequestID == strings.Repeat("x", 129) {
		t.Fatalf("status=%d requestID=%q", recorder.Code, controller.actor.RequestID)
	}

	// >>> 数据演变示例
	// 1. JSON对象后追加{} -> 400 invalid_request。
	// 2. 129字符Request-ID -> 服务端随机ID -> 审计字段安全。
}

// TestRemovedForbiddenMonitorRoutesAreNotRegistered 防止已废弃的训练样本和组合规则接口重新暴露。
func TestRemovedForbiddenMonitorRoutesAreNotRegistered(t *testing.T) {
	monitor := &fakeForbiddenMonitor{}
	server, token := newMonitorTestServer(t, monitor)
	targets := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/plugins/forbidden_message_monitor/training-samples"},
		{method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/training-samples", body: `{"msg_content":"广告","trial_id":"trial-1"}`},
		{method: http.MethodDelete, path: "/api/plugins/forbidden_message_monitor/training-samples/1", body: `{"expected_version":1}`},
		{method: http.MethodGet, path: "/api/plugins/forbidden_message_monitor/combinations"},
		{method: http.MethodPost, path: "/api/plugins/forbidden_message_monitor/combinations", body: `{"terms":["加","微信"],"bonus":30}`},
		{method: http.MethodDelete, path: "/api/plugins/forbidden_message_monitor/combinations/1", body: `{"expected_version":1}`},
	}
	for _, target := range targets {
		recorder := requestAPI(server, token, target.method, target.path, target.body, "req-removed")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s %s status=%d body=%s", target.method, target.path, recorder.Code, recorder.Body.String())
		}
	}
	if len(monitor.calls) != 0 {
		t.Fatalf("removed route reached service: %v", monitor.calls)
	}
}

// requestLogin 使用固定测试远端地址执行登录请求。
// @param server：测试服务；qq：登录QQ；password：登录密码。
// @returns 已完成的登录响应记录器。
// ⚠️副作用说明：执行内存 HTTP 请求并可能运行 Argon2id。
func requestLogin(server *Server, qq string, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"qq": qq, "password": password})
	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(string(body)))
	request.RemoteAddr = "192.0.2.1:12345"
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	// >>> 数据演变示例
	// 1. QQ100+正确密码 -> 200记录器。
	// 2. QQ100+错误密码 -> 401或限流429记录器。
	return recorder
}

// requestAPI 执行携带管理员 Token 的通用内存 API 请求。
// @param server：测试服务；token：JWT；method、path、body：请求参数；requestID：审计请求ID。
// @returns 已完成的响应记录器。
// ⚠️副作用说明：创建并执行内存 HTTP 请求。
func requestAPI(server *Server, token string, method string, path string, body string, requestID string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	// [决策理由] 非空请求ID才写入请求头，覆盖客户端提供与服务端生成两种路径。
	if requestID != "" {
		request.Header.Set("X-Request-ID", requestID)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	// >>> 数据演变示例
	// 1. GET /api/commands+Token -> 200记录器。
	// 2. DELETE /api/commands/abc+Token -> 400记录器。
	return recorder
}
