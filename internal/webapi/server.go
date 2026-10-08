// 📌 影响范围：读取系统时间和 crypto/rand；校验管理员快照；签发及验证 HMAC-SHA256 JWT；写入 HTTP 响应与访问日志。
package webapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/w1ndys/w1ndys-bot/internal/admin"
	projectauth "github.com/w1ndys/w1ndys-bot/internal/auth"
	"github.com/w1ndys/w1ndys-bot/internal/management"
	projectlogger "github.com/w1ndys/w1ndys-bot/pkg/logger"
)

const tokenLifetime = 12 * time.Hour
const loginWindow = time.Minute
const loginAttemptLimit = 5
const loginAttemptCapacity = 4096

// ApplicationName 是 WebUI 固定展示名称，不开放运行时修改。
const ApplicationName = "w1ndys-bot-webui"

type contextKey string

const actorContextKey contextKey = "webui_actor"
const requestIDContextKey contextKey = "request_id"
const managementActorContextKey contextKey = "management_actor"

// AdminResolver 定义 WebUI 登录及请求期间所需的管理员快照能力。
type AdminResolver interface {
	Resolve(string) (admin.SystemAdmin, bool)
}

// ManagementController 定义平台级设置与审计管理能力。
type ManagementController interface {
	ListSettings(context.Context, management.Actor) ([]management.SettingState, error)
	SetSetting(context.Context, management.Actor, string, json.RawMessage) (management.SettingState, error)
	DeleteSetting(context.Context, management.Actor, string) error
	ListAuditLogs(context.Context, management.Actor, management.AuditQuery) (management.AuditPage, error)
	GetAuditLog(context.Context, management.Actor, int64) (management.AuditState, error)
}

// Server 提供 WebUI 认证 HTTP 接口。
type Server struct {
	passwordHash     string
	jwtSecret        []byte
	admins           AdminResolver
	management       ManagementController
	runtimes         RuntimeStateController
	keywordReply     KeywordReplyController
	forbiddenMonitor ForbiddenMonitorController
	now              func() time.Time
	loginMu          sync.Mutex
	loginAttempts    map[string]loginAttempt
	loginSlots       chan struct{}
}

type loginAttempt struct {
	Count       int
	WindowStart time.Time
}

type loginRequest struct {
	QQ       string `json:"qq"`
	Password string `json:"password"`
}

type tokenClaims struct {
	Subject   string `json:"sub"`
	ExpiresAt int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
}

type responseEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

type settingSetRequest struct {
	Value json.RawMessage `json:"value"`
}

type settingResponse struct {
	Key         string          `json:"key"`
	Value       json.RawMessage `json:"value"`
	Description string          `json:"description"`
	Overridden  bool            `json:"overridden"`
}

type auditResponse struct {
	ID           int64           `json:"id"`
	ActorID      string          `json:"actor_id"`
	ActorRole    string          `json:"actor_role"`
	Channel      string          `json:"channel"`
	Action       string          `json:"action"`
	TargetType   string          `json:"target_type"`
	TargetID     string          `json:"target_id"`
	Before       json.RawMessage `json:"before"`
	After        json.RawMessage `json:"after"`
	Success      bool            `json:"success"`
	ErrorMessage string          `json:"error_message"`
	RequestID    string          `json:"request_id"`
	CreatedAt    time.Time       `json:"created_at"`
}

type auditPageResponse struct {
	Items    []auditSummaryResponse `json:"items"`
	Page     int                    `json:"page"`
	PageSize int                    `json:"page_size"`
	Total    int64                  `json:"total"`
}

type auditSummaryResponse struct {
	ID           int64     `json:"id"`
	ActorID      string    `json:"actor_id"`
	ActorRole    string    `json:"actor_role"`
	Channel      string    `json:"channel"`
	Action       string    `json:"action"`
	TargetType   string    `json:"target_type"`
	TargetID     string    `json:"target_id"`
	Success      bool      `json:"success"`
	ErrorMessage string    `json:"error_message"`
	RequestID    string    `json:"request_id"`
	CreatedAt    time.Time `json:"created_at"`
}

// New 创建 WebUI API 服务并在内存中准备环境密码哈希。
// @param password：环境变量中的共享密码；jwtSecret：JWT 签名密钥；admins：管理员快照解析器；controller：管理业务服务；runtimes：目标插件开关服务。
// @returns 可注册到 HTTP 路由的 Server，或配置强度错误。
// ⚠️副作用说明：读取系统加密随机源并执行 Argon2id 哈希。
func New(password string, jwtSecret string, admins AdminResolver, controller ManagementController, runtimes RuntimeStateController, keywordReply KeywordReplyController, forbiddenMonitor ForbiddenMonitorController) (*Server, error) {
	// [决策理由] JWT 密钥过短会降低离线伪造成本，必须在监听端口前拒绝启动。
	if len([]byte(jwtSecret)) < 32 {
		return nil, errors.New("JWT_SECRET 不能少于32字节")
	}
	// [决策理由] 管理员解析器缺失时无法完成服务端授权确认。
	if admins == nil {
		return nil, errors.New("管理员解析器不能为空")
	}
	// [决策理由] 插件控制器缺失时管理路由会在鉴权后崩溃，必须在组装阶段拒绝。
	if controller == nil {
		return nil, errors.New("管理服务不能为空")
	}
	// [决策理由] 目标插件开关服务缺失时运行时路由会在鉴权后崩溃。
	if runtimes == nil {
		return nil, errors.New("插件开关服务不能为空")
	}
	// [决策理由] 专属插件服务缺失时对应管理路由会在鉴权后崩溃。
	if keywordReply == nil {
		return nil, errors.New("关键词回复服务不能为空")
	}
	if forbiddenMonitor == nil {
		return nil, errors.New("违禁消息监控服务不能为空")
	}
	passwordHash, err := projectauth.HashPassword(password)
	// [决策理由] 环境密码不符合强度要求或随机源失败时禁止开放登录接口。
	if err != nil {
		return nil, fmt.Errorf("准备 WebUI 密码: %w", err)
	}
	server := &Server{passwordHash: passwordHash, jwtSecret: []byte(jwtSecret), admins: admins, management: controller, runtimes: runtimes, keywordReply: keywordReply, forbiddenMonitor: forbiddenMonitor, now: time.Now, loginAttempts: make(map[string]loginAttempt), loginSlots: make(chan struct{}, 2)}

	// >>> 数据演变示例
	// 1. 强密码+32字节密钥+Resolver -> Argon2id哈希 -> Server,nil。
	// 2. 短JWT密钥 -> 强度检查 -> nil,error。
	return server, nil
}

// Handler 返回包含登录、当前身份和通用中间件的 HTTP 路由。
// @param 无。
// @returns 可挂载在 /api/ 路径下的 http.Handler。
// ⚠️副作用说明：创建内存路由；请求到达后会写响应和访问日志。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.Handle("GET /api/auth/me", s.authenticate(http.HandlerFunc(s.me)))
	mux.Handle("GET /api/plugin-runtimes", s.authenticate(http.HandlerFunc(s.listPluginRuntimes)))
	mux.Handle("GET /api/plugin-runtimes/{plugin_key}", s.authenticate(http.HandlerFunc(s.getPluginRuntime)))
	mux.Handle("PATCH /api/plugin-runtimes/{plugin_key}", s.authenticate(http.HandlerFunc(s.patchPluginRuntime)))
	mux.Handle("PUT /api/plugin-runtimes/{plugin_key}/groups/{group_id}", s.authenticate(http.HandlerFunc(s.putPluginRuntimeGroup)))
	mux.Handle("POST /api/plugin-runtimes/group-maintenance/copy/preview", s.authenticate(http.HandlerFunc(s.previewPluginRuntimeGroupCopy)))
	mux.Handle("POST /api/plugin-runtimes/group-maintenance/copy", s.authenticate(http.HandlerFunc(s.copyPluginRuntimeGroups)))
	mux.Handle("POST /api/plugin-runtimes/group-maintenance/stale/preview", s.authenticate(http.HandlerFunc(s.previewStalePluginRuntimeGroups)))
	mux.Handle("POST /api/plugin-runtimes/group-maintenance/stale/cleanup", s.authenticate(http.HandlerFunc(s.cleanupStalePluginRuntimeGroups)))
	mux.Handle("GET /api/plugins/keyword_reply/groups/{group_id}/rules", s.authenticate(http.HandlerFunc(s.listKeywordRules)))
	mux.Handle("POST /api/plugins/keyword_reply/groups/{group_id}/rules", s.authenticate(http.HandlerFunc(s.createKeywordRule)))
	mux.Handle("PUT /api/plugins/keyword_reply/groups/{group_id}/rules/{rule_id}", s.authenticate(http.HandlerFunc(s.updateKeywordRule)))
	mux.Handle("DELETE /api/plugins/keyword_reply/groups/{group_id}/rules/{rule_id}", s.authenticate(http.HandlerFunc(s.deleteKeywordRule)))
	mux.Handle("GET /api/plugins/forbidden_message_monitor/violations", s.authenticate(http.HandlerFunc(s.listViolations)))
	mux.Handle("POST /api/plugins/forbidden_message_monitor/violations/{violation_id}/review", s.authenticate(http.HandlerFunc(s.reviewViolation)))
	mux.Handle("POST /api/plugins/forbidden_message_monitor/text-trials", s.authenticate(http.HandlerFunc(s.createTextTrial)))
	mux.Handle("GET /api/plugins/forbidden_message_monitor/terms", s.authenticate(http.HandlerFunc(s.listTerms)))
	mux.Handle("POST /api/plugins/forbidden_message_monitor/terms", s.authenticate(http.HandlerFunc(s.createTerm)))
	mux.Handle("PUT /api/plugins/forbidden_message_monitor/terms/{term_id}", s.authenticate(http.HandlerFunc(s.updateTerm)))
	mux.Handle("DELETE /api/plugins/forbidden_message_monitor/terms/{term_id}", s.authenticate(http.HandlerFunc(s.deleteTerm)))
	mux.Handle("GET /api/plugin-runtimes/{plugin_key}/config", s.authenticate(http.HandlerFunc(s.getPluginRuntimeConfig)))
	mux.Handle("PUT /api/plugin-runtimes/{plugin_key}/config", s.authenticate(http.HandlerFunc(s.putPluginRuntimeConfig)))
	mux.Handle("GET /api/settings", s.authenticate(http.HandlerFunc(s.listSettings)))
	mux.Handle("PUT /api/settings/{setting_key}", s.authenticate(http.HandlerFunc(s.setSetting)))
	mux.Handle("DELETE /api/settings/{setting_key}", s.authenticate(http.HandlerFunc(s.deleteSetting)))
	mux.Handle("GET /api/audit-logs", s.authenticate(http.HandlerFunc(s.listAuditLogs)))
	mux.Handle("GET /api/audit-logs/{audit_id}", s.authenticate(http.HandlerFunc(s.getAuditLog)))
	handler := s.middleware(mux)

	// >>> 数据演变示例
	// 1. POST /api/auth/login -> login处理器。
	// 2. GET /api/auth/me无Token -> authenticate -> 401。
	return handler
}

// login 校验管理员 QQ 与共享密码并签发 JWT。
// @param writer：响应写入器；request：JSON 登录请求。
// @returns 无。
// ⚠️副作用说明：读取请求体、执行 Argon2id 校验并写入 JSON 响应。
func (s *Server) login(writer http.ResponseWriter, request *http.Request) {
	var input loginRequest
	// [决策理由] 登录载荷必须是单个、字段明确的 JSON 对象，避免歧义输入。
	if err := decodeJSON(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "登录参数格式无效")
		return
	}
	release, allowed := s.beginLogin(request)
	// [决策理由] 超过失败窗口或 Argon2 并发上限时必须在高成本哈希前拒绝。
	if !allowed {
		writeError(writer, http.StatusTooManyRequests, "login_rate_limited", "登录尝试过于频繁，请稍后重试")
		return
	}
	defer release()
	account, exists := s.admins.Resolve(input.QQ)
	matched, verifyErr := projectauth.VerifyPassword(input.Password, s.passwordHash)
	// [决策理由] QQ 与密码使用同一模糊错误，避免暴露管理员账号是否存在。
	if verifyErr != nil || !matched || !exists {
		writeError(writer, http.StatusUnauthorized, "invalid_credentials", "QQ号或密码错误")
		return
	}
	s.clearLoginAttempts(request)
	token, err := s.sign(input.QQ)
	// [决策理由] 签名失败表示服务器无法建立可信会话，不能返回部分登录结果。
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "internal_error", "签发登录凭证失败")
		return
	}
	writeSuccess(writer, map[string]any{"token": token, "expires_in": int64(tokenLifetime.Seconds()), "admin": account})

	// >>> 数据演变示例
	// 1. 启用管理员+正确密码 -> JWT -> 200及管理员信息。
	// 2. 非管理员+正确密码 -> 固定认证失败响应 -> 401。
}

// beginLogin 对客户端 IP 执行固定窗口限流和 Argon2 并发保护。
// @param request：登录请求。
// @returns 释放并发槽函数，以及是否允许继续验证。
// ⚠️副作用说明：修改进程内登录尝试计数，并可能占用 Argon2 并发槽。
func (s *Server) beginLogin(request *http.Request) (func(), bool) {
	key := loginKey(request)
	now := s.now()
	s.loginMu.Lock()
	// [决策理由] 达到容量前先淘汰过期窗口，防止大量来源永久增长内存。
	if len(s.loginAttempts) >= loginAttemptCapacity {
		s.cleanupLoginAttemptsLocked(now)
	}
	_, known := s.loginAttempts[key]
	// [决策理由] 清理后仍满载时拒绝新来源，保证 map 具有硬容量上限。
	if !known && len(s.loginAttempts) >= loginAttemptCapacity {
		s.loginMu.Unlock()
		return func() {}, false
	}
	attempt := s.loginAttempts[key]
	// [决策理由] 新窗口应重置历史失败次数，避免永久锁定管理员。
	if attempt.WindowStart.IsZero() || now.Sub(attempt.WindowStart) >= loginWindow {
		attempt = loginAttempt{WindowStart: now}
	}
	// [决策理由] 固定窗口最多允许五次高成本密码验证。
	if attempt.Count >= loginAttemptLimit {
		s.loginMu.Unlock()
		return func() {}, false
	}
	select {
	case s.loginSlots <- struct{}{}:
		attempt.Count++
		s.loginAttempts[key] = attempt
		s.loginMu.Unlock()
		release := func() {
			<-s.loginSlots

			// >>> 数据演变示例
			// 1. 已占用槽 -> release -> 槽位归还。
			// 2. defer调用 -> Argon2结束后释放。
		}

		// >>> 数据演变示例
		// 1. IP首次登录且有槽 -> 计数1 -> release,true。
		// 2. 窗口内第六次 -> 不占槽 -> false。
		return release, true
	default:
		s.loginMu.Unlock()
		return func() {}, false
	}
}

// clearLoginAttempts 在成功登录后清除对应限流窗口。
// @param request：成功登录请求。
// @returns 无。
// ⚠️副作用说明：删除进程内登录尝试计数。
func (s *Server) clearLoginAttempts(request *http.Request) {
	s.loginMu.Lock()
	delete(s.loginAttempts, loginKey(request))
	s.loginMu.Unlock()

	// >>> 数据演变示例
	// 1. key计数3+登录成功 -> delete -> 下次从0开始。
	// 2. key不存在 -> delete无影响。
}

// cleanupLoginAttemptsLocked 删除当前窗口前已经过期的登录计数。
// @param now：当前服务时间。
// @returns 无。
// ⚠️副作用说明：要求调用方持有 loginMu，并删除过期 map 项。
func (s *Server) cleanupLoginAttemptsLocked(now time.Time) {
	for key, attempt := range s.loginAttempts {
		// [决策理由] 只删除完整窗口外记录，当前限流语义保持不变。
		if now.Sub(attempt.WindowStart) >= loginWindow {
			delete(s.loginAttempts, key)
		}
	}

	// >>> 数据演变示例
	// 1. [旧IP:2,当前IP:3] -> 删除旧IP -> 保留当前IP。
	// 2. 所有记录过期 -> map清空。
}

// loginKey 构造不信任代理头的 IP 限流键。
// @param request：登录请求。
// @returns TCP RemoteAddr 中的 IP；非标准地址回退原文。
// ⚠️副作用说明：无。
func loginKey(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	// [决策理由] 测试或非标准服务器可能没有端口，应回退完整 RemoteAddr。
	if err != nil {
		host = request.RemoteAddr
	}

	// >>> 数据演变示例
	// 1. 1.2.3.4:5000 -> 1.2.3.4。
	// 2. RemoteAddr=test -> test。
	return host
}

// me 返回 JWT 对应的当前启用管理员身份。
// @param writer：响应写入器；request：已通过鉴权且携带 Actor 的请求。
// @returns 无。
// ⚠️副作用说明：读取请求上下文并写入 JSON 响应。
func (s *Server) me(writer http.ResponseWriter, request *http.Request) {
	actor, exists := request.Context().Value(actorContextKey).(admin.SystemAdmin)
	// [决策理由] 鉴权中间件未注入身份表示路由装配错误，按未授权安全失败。
	if !exists {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "登录凭证无效")
		return
	}
	writeSuccess(writer, actor)

	// >>> 数据演变示例
	// 1. context含管理员100 -> 200 data{user_id:100}。
	// 2. context无身份 -> 401 unauthorized。
}

// listSettings 返回全部受控系统设置及当前有效值。
// @param writer：响应写入器；request：已鉴权请求。
// @returns 无。
// ⚠️副作用说明：读取 PostgreSQL 设置覆盖并写入 JSON 响应。
func (s *Server) listSettings(writer http.ResponseWriter, request *http.Request) {
	actor := actorFromRequest(request)
	states, err := s.management.ListSettings(request.Context(), actor)
	// [决策理由] 设置列表需要合并完整默认值，查询失败时不能返回部分结果。
	if err != nil {
		writeManagementError(writer, err)
		return
	}
	result := make([]settingResponse, 0, len(states))
	for _, state := range states {
		result = append(result, settingView(state))
	}
	writeSuccess(writer, result)

	// >>> 数据演变示例
	// 1. DB覆盖prefix=!+其余默认 -> 合并DTO数组 -> 200。
	// 2. Repository失败 -> 500统一错误且不返回部分设置。
}

// setSetting 保存一个已注册系统设置的 JSON 值。
// @param writer：响应写入器；request：携带设置键与 value 的已鉴权请求。
// @returns 无。
// ⚠️副作用说明：可能 UPSERT 设置、写入审计并热刷新 SettingsResolver。
func (s *Server) setSetting(writer http.ResponseWriter, request *http.Request) {
	actor := actorFromRequest(request)
	key := request.PathValue("setting_key")
	var input settingSetRequest
	// [决策理由] value 必须保持原始 JSON 类型，未知包装字段应明确拒绝。
	if err := decodeJSON(writer, request, &input); err != nil || len(input.Value) == 0 {
		writeError(writer, http.StatusBadRequest, "invalid_request", "系统设置参数格式无效")
		return
	}
	state, err := s.management.SetSetting(request.Context(), actor, key, input.Value)
	// [决策理由] 未知键、类型范围和持久化错误需要稳定区分。
	if err != nil {
		writeManagementError(writer, err)
		return
	}
	writeSuccess(writer, settingView(state))

	// >>> 数据演变示例
	// 1. command_prefix+value="!" -> 校验+UPSERT+审计+热刷新 -> 200。
	// 2. default_page_size+value=500 -> invalid_setting -> 400。
}

// deleteSetting 删除数据库设置覆盖并回退定义默认值。
// @param writer：响应写入器；request：携带设置键的已鉴权请求。
// @returns 无。
// ⚠️副作用说明：可能删除设置覆盖、写入审计并热刷新 SettingsResolver。
func (s *Server) deleteSetting(writer http.ResponseWriter, request *http.Request) {
	actor := actorFromRequest(request)
	key := request.PathValue("setting_key")
	// [决策理由] 删除失败时不能声称已恢复默认值。
	if err := s.management.DeleteSetting(request.Context(), actor, key); err != nil {
		writeManagementError(writer, err)
		return
	}
	writeSuccess(writer, nil)

	// >>> 数据演变示例
	// 1. command_prefix存在覆盖 -> DELETE+审计+Load -> 200 data:null并回退/。
	// 2. 未覆盖设置 -> setting_override_not_found -> 404。
}

// settingView 将内部设置状态转换成稳定 API 模型。
// @param state：管理服务设置状态。
// @returns 包含当前值和是否覆盖的 snake_case 设置响应。
// ⚠️副作用说明：复制 JSON 值，避免响应持有共享切片。
func settingView(state management.SettingState) settingResponse {
	value := append(json.RawMessage(nil), state.Value...)
	// [决策理由] 设置值理论上始终合法，空值仍安全输出 null 而非破坏整个响应编码。
	if len(value) == 0 {
		value = json.RawMessage(`null`)
	}
	view := settingResponse{Key: state.Key, Value: value, Description: state.Description, Overridden: state.Overridden}

	// >>> 数据演变示例
	// 1. prefix="!"+Overridden=true -> DTO保留JSON字符串和覆盖标记。
	// 2. 空Value -> null -> 保持合法统一响应。
	return view
}

// listAuditLogs 返回支持分页和筛选的只读审计列表。
// @param writer：响应写入器；request：携带可选查询参数的已鉴权请求。
// @returns 无。
// ⚠️副作用说明：执行 PostgreSQL 审计计数与分页查询并写入 JSON 响应。
func (s *Server) listAuditLogs(writer http.ResponseWriter, request *http.Request) {
	actor := actorFromRequest(request)
	query, err := parseAuditQuery(request)
	// [决策理由] 查询参数格式错误应在访问数据库前返回400。
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_audit_query", "审计查询参数无效")
		return
	}
	page, err := s.management.ListAuditLogs(request.Context(), actor, query)
	// [决策理由] 授权、参数边界或数据库错误需要统一领域映射。
	if err != nil {
		writeManagementError(writer, err)
		return
	}
	items := make([]auditSummaryResponse, 0, len(page.Items))
	for _, state := range page.Items {
		items = append(items, auditSummaryView(state))
	}
	writeSuccess(writer, auditPageResponse{Items: items, Page: page.Page, PageSize: page.PageSize, Total: page.Total})

	// >>> 数据演变示例
	// 1. page=1&page_size=20&actor_id=100 -> 筛选分页 -> 200 AuditPage。
	// 2. start_time=bad -> 参数解析失败 -> 400零数据库访问。
}

// getAuditLog 返回指定审计记录的完整前后快照。
// @param writer：响应写入器；request：携带审计 ID 的已鉴权请求。
// @returns 无。
// ⚠️副作用说明：执行 PostgreSQL 单条只读查询并写入 JSON 响应。
func (s *Server) getAuditLog(writer http.ResponseWriter, request *http.Request) {
	actor := actorFromRequest(request)
	id, err := parsePositiveID(request.PathValue("audit_id"))
	// [决策理由] 非正整数路径不能定位审计主键，应在查询前拒绝。
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "审计日志 ID 格式无效")
		return
	}
	state, err := s.management.GetAuditLog(request.Context(), actor, id)
	// [决策理由] 未找到和数据库错误必须返回不同业务码。
	if err != nil {
		writeManagementError(writer, err)
		return
	}
	writeSuccess(writer, auditView(state))

	// >>> 数据演变示例
	// 1. id8存在 -> 查询完整before/after -> 200。
	// 2. id404 -> ErrAuditNotFound -> 404 audit_not_found。
}

func parsePositiveID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("ID must be a positive integer")
	}
	return id, nil
}

// parseAuditQuery 解析审计分页、精确筛选和时间范围。
// @param request：HTTP 查询请求。
// @returns 带默认分页的 AuditQuery 或格式错误。
// ⚠️副作用说明：无；仅读取 URL 查询参数。
func parseAuditQuery(request *http.Request) (management.AuditQuery, error) {
	values := request.URL.Query()
	query := management.AuditQuery{Page: 1, PageSize: 20, ActorID: values.Get("actor_id"), Action: values.Get("action"), TargetType: values.Get("target_type"), TargetID: values.Get("target_id")}
	// [决策理由] 非空 page 必须是十进制整数。
	if raw := values.Get("page"); raw != "" {
		value, err := strconv.Atoi(raw)
		// [决策理由] 解析失败无法形成稳定分页偏移。
		if err != nil {
			return management.AuditQuery{}, err
		}
		query.Page = value
	}
	// [决策理由] 非空 page_size 必须是十进制整数。
	if raw := values.Get("page_size"); raw != "" {
		value, err := strconv.Atoi(raw)
		// [决策理由] 解析失败无法形成稳定 LIMIT。
		if err != nil {
			return management.AuditQuery{}, err
		}
		query.PageSize = value
	}
	startTime, err := parseOptionalTime(values.Get("start_time"))
	// [决策理由] 起始时间必须使用 RFC3339，避免时区歧义。
	if err != nil {
		return management.AuditQuery{}, err
	}
	endTime, err := parseOptionalTime(values.Get("end_time"))
	// [决策理由] 结束时间必须使用 RFC3339，避免服务器本地时区猜测。
	if err != nil {
		return management.AuditQuery{}, err
	}
	query.StartTime, query.EndTime = startTime, endTime

	// >>> 数据演变示例
	// 1. 无参数 -> page1,size20,空筛选。
	// 2. page=2&start_time=RFC3339 -> page2+时间指针。
	return query, nil
}

// parseOptionalTime 解析可为空的 RFC3339 时间。
// @param value：查询参数时间原文。
// @returns 空值对应 nil，非空对应 UTC 含时区时间或错误。
// ⚠️副作用说明：无。
func parseOptionalTime(value string) (*time.Time, error) {
	// [决策理由] 空筛选表示不限制该时间边界。
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	// [决策理由] 非 RFC3339 时间缺少稳定时区语义，必须拒绝。
	if err != nil {
		return nil, err
	}
	utc := parsed.UTC()

	// >>> 数据演变示例
	// 1. "" -> nil,nil。
	// 2. 2026-07-13T10:00:00+08:00 -> 2026-07-13T02:00:00Z。
	return &utc, nil
}

// auditView 将内部审计状态转换成稳定 API 模型。
// @param state：管理服务审计状态。
// @returns JSON 快照为 null 或原值的 snake_case 审计响应。
// ⚠️副作用说明：复制前后 JSON 字节。
func auditView(state management.AuditState) auditResponse {
	before := redactAuditJSON(state.BeforeJSON)
	after := redactAuditJSON(state.AfterJSON)
	// [决策理由] 数据库 NULL 快照应编码成 JSON null，而不是省略或破坏响应。
	if len(before) == 0 {
		before = json.RawMessage(`null`)
	}
	// [决策理由] 数据库 NULL 后快照通常表示删除操作，应明确输出 null。
	if len(after) == 0 {
		after = json.RawMessage(`null`)
	}
	view := auditResponse{ID: state.ID, ActorID: state.ActorID, ActorRole: state.ActorRole, Channel: state.Channel, Action: state.Action, TargetType: state.TargetType, TargetID: state.TargetID, Before: before, After: after, Success: state.Success, ErrorMessage: sanitizeAuditError(state.ErrorMessage), RequestID: state.RequestID, CreatedAt: state.CreatedAt.UTC()}

	// >>> 数据演变示例
	// 1. update含before/after+东八区时间 -> DTO保留JSON并转UTC。
	// 2. delete的after为空 -> after:null。
	return view
}

// auditSummaryView 将内部审计状态转换为不含快照的列表摘要。
// @param state：管理服务审计状态。
// @returns 不包含before/after大字段的只读摘要。
// ⚠️副作用说明：无。
func auditSummaryView(state management.AuditState) auditSummaryResponse {
	view := auditSummaryResponse{ID: state.ID, ActorID: state.ActorID, ActorRole: state.ActorRole, Channel: state.Channel, Action: state.Action, TargetType: state.TargetType, TargetID: state.TargetID, Success: state.Success, ErrorMessage: sanitizeAuditError(state.ErrorMessage), RequestID: state.RequestID, CreatedAt: state.CreatedAt.UTC()}

	// >>> 数据演变示例
	// 1. 含大型before/after的记录 -> 仅复制元数据 -> 轻量摘要。
	// 2. 东八区created_at -> UTC转换 -> Z结尾时间。
	return view
}

// redactAuditJSON 递归脱敏审计快照中的常见凭据字段。
// @param raw：数据库保存的JSON快照。
// @returns 脱敏后的独立JSON；空值或解析失败返回null。
// ⚠️副作用说明：分配新JSON数据，不修改数据库原始审计记录。
func redactAuditJSON(raw json.RawMessage) json.RawMessage {
	// [决策理由] 数据库NULL快照应稳定输出JSON null。
	if len(raw) == 0 {
		return json.RawMessage(`null`)
	}
	var value any
	// [决策理由] 非法历史JSON不得原样回传而泄露潜在敏感文本。
	if err := json.Unmarshal(raw, &value); err != nil {
		return json.RawMessage(`null`)
	}
	redacted := redactAuditValue(value)
	encoded, err := json.Marshal(redacted)
	// [决策理由] 无法重新编码时必须采用安全闭合的null响应。
	if err != nil {
		return json.RawMessage(`null`)
	}

	// >>> 数据演变示例
	// 1. {"token":"abc"} -> 解析并递归脱敏 -> {"token":"[已脱敏]"}。
	// 2. 空值或非法JSON -> 安全回退 -> null。
	return encoded
}

// redactAuditValue 递归复制JSON值并替换敏感字段。
// @param value：已解析JSON值。
// @returns 保持结构的脱敏副本。
// ⚠️副作用说明：分配新的map与slice，不修改输入值。
func redactAuditValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			// [决策理由] 名称包含凭据语义的字段不允许离开服务端。
			if isSensitiveAuditKey(key) {
				result[key] = "[已脱敏]"
			} else {
				result[key] = redactAuditValue(child)
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = redactAuditValue(child)
		}
		return result
	default:
		// >>> 数据演变示例
		// 1. map含嵌套secret -> 递归map -> 字段替换。
		// 2. 数组含普通数字与布尔值 -> 逐项复制 -> 原值保持。
		return typed
	}
}

// isSensitiveAuditKey 判断JSON字段名是否表达常见凭据语义。
// @param key：原始字段名，可能包含大小写及分隔符。
// @returns 规范化后命中敏感词时为true。
// ⚠️副作用说明：无。
func isSensitiveAuditKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
	sensitiveTerms := []string{"password", "token", "secret", "credential", "apikey", "accesskey", "privatekey", "authorization", "cookie", "session", "dsn"}
	for _, term := range sensitiveTerms {
		// [决策理由] 前后缀组合如database_dsn和session_id同样属于敏感字段。
		if strings.Contains(normalized, term) {
			return true
		}
	}

	// >>> 数据演变示例
	// 1. "API_Key" -> "apikey" -> 命中apikey -> true。
	// 2. "enabled" -> "enabled" -> 无敏感词 -> false。
	return false
}

// sanitizeAuditError 避免数据库错误文本把请求凭据带到管理页面。
// @param message：内部审计错误原文。
// @returns 空错误保持为空，非空错误返回可关联请求ID的固定说明。
// ⚠️副作用说明：无。
func sanitizeAuditError(message string) string {
	// [决策理由] 失败详情可能拼接外部输入，API不能安全判断其中哪些片段是秘密。
	if message != "" {
		return "操作失败，请根据请求 ID 查看受限服务端日志"
	}

	// >>> 数据演变示例
	// 1. "连接失败 password=abc" -> 非空 -> 固定安全说明。
	// 2. "" -> 无失败详情 -> ""。
	return ""
}

// actorFromRequest 将认证身份与请求 ID 转换成管理服务 Actor。
// @param request：已通过 authenticate 和 middleware 的请求。
// @returns 认证中间件注入的 WebUI 最高管理员 Actor。
// ⚠️副作用说明：无；仅读取请求上下文。
func actorFromRequest(request *http.Request) management.Actor {
	actor, _ := request.Context().Value(managementActorContextKey).(management.Actor)

	// >>> 数据演变示例
	// 1. authenticate注入Actor100:req-abc -> 返回可信Actor。
	// 2. 未经路由中间件直接调用Handler -> 返回零值并由Service再次拒绝。
	return actor
}

// writeManagementError 将平台管理领域错误映射为稳定 HTTP 状态和错误码。
func writeManagementError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, admin.ErrInvalidSetting):
		writeError(writer, http.StatusBadRequest, "invalid_setting", "系统设置值无效")
	case errors.Is(err, admin.ErrUnknownSetting):
		writeError(writer, http.StatusNotFound, "unknown_setting", "未知系统设置")
	case errors.Is(err, admin.ErrSettingNotFound):
		writeError(writer, http.StatusNotFound, "setting_override_not_found", "系统设置当前没有覆盖值")
	case errors.Is(err, admin.ErrInvalidAuditQuery):
		writeError(writer, http.StatusBadRequest, "invalid_audit_query", "审计查询参数无效")
	case errors.Is(err, admin.ErrAuditNotFound):
		writeError(writer, http.StatusNotFound, "audit_not_found", "审计日志不存在")
	case errors.Is(err, admin.ErrForbidden), errors.Is(err, admin.ErrInvalidActor), errors.Is(err, admin.ErrInvalidChannel):
		writeError(writer, http.StatusForbidden, "forbidden", "无权执行该管理操作")
	default:
		projectlogger.Error("WebAPI管理操作失败", "error", err)
		writeError(writer, http.StatusInternalServerError, "internal_error", "管理操作执行失败")
	}
}

// decodeJSON 解码单个、字段严格且大小受限的 JSON 请求体。
// @param writer：用于 MaxBytesReader；request：HTTP 请求；target：目标结构体指针。
// @returns 首次解码、未知字段、超限或尾随内容错误。
// ⚠️副作用说明：读取并消费请求体。
func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	// 文本试判允许 4000 个 Unicode 字符；16 KiB 同时覆盖 JSON 转义开销并保持明确载荷上限。
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 16<<10))
	decoder.DisallowUnknownFields()
	// [决策理由] 第一个 JSON 值必须完整匹配目标结构。
	if err := decoder.Decode(target); err != nil {
		return err
	}
	// [决策理由] 第二次解码必须到达 EOF，拒绝 `{...}{...}` 等尾随 JSON。
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("请求体只能包含一个 JSON 值")
	}

	// >>> 数据演变示例
	// 1. {"enabled":true} -> 首次成功+EOF -> nil。
	// 2. {"enabled":true}{} -> 第二次有值 -> error。
	return nil
}

// authenticate 验证 Bearer JWT 并再次确认管理员仍处于启用状态。
// @param next：鉴权成功后调用的处理器。
// @returns 包装后的 HTTP 处理器。
// ⚠️副作用说明：读取请求头和管理员快照；失败时写入401响应。
func (s *Server) authenticate(next http.Handler) http.Handler {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := request.Header.Get("Authorization")
		// [决策理由] 仅接受标准 Bearer 方案，避免把其他认证头误作 JWT。
		if !strings.HasPrefix(header, "Bearer ") {
			writeError(writer, http.StatusUnauthorized, "unauthorized", "缺少登录凭证")
			return
		}
		claims, err := s.verify(strings.TrimPrefix(header, "Bearer "))
		// [决策理由] 签名、结构或有效期任一失败都不能建立可信身份。
		if err != nil {
			writeError(writer, http.StatusUnauthorized, "unauthorized", "登录凭证无效或已过期")
			return
		}
		account, exists := s.admins.Resolve(claims.Subject)
		// [决策理由] 每次请求查询内存快照，使管理员禁用后已签发 Token 立即失效。
		if !exists {
			writeError(writer, http.StatusUnauthorized, "unauthorized", "管理员账号已停用")
			return
		}
		ctx := context.WithValue(request.Context(), actorContextKey, account)
		requestID, _ := request.Context().Value(requestIDContextKey).(string)
		actor := management.Actor{ID: account.UserID, Role: "super_admin", Channel: management.ChannelWebUI, RequestID: requestID}
		ctx = context.WithValue(ctx, managementActorContextKey, actor)
		next.ServeHTTP(writer, request.WithContext(ctx))

		// >>> 数据演变示例
		// 1. 有效Token+启用管理员 -> 注入account和审计Actor -> next。
		// 2. 有效Token+管理员已禁用 -> 401 -> 不调用next。
	})

	// >>> 数据演变示例
	// 1. me处理器 -> authenticate包装 -> 受保护处理器。
	// 2. nil身份请求 -> 包装器校验失败 -> 401。
	return handler
}

// sign 为管理员签发固定12小时有效的 HMAC JWT。
// @param subject：管理员 QQ 号。
// @returns JWT 字符串或 JSON 编码错误。
// ⚠️副作用说明：读取当前时间。
func (s *Server) sign(subject string) (string, error) {
	now := s.now()
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	// [决策理由] 虽然静态头通常不会失败，仍需保持签名链路错误完整。
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(tokenClaims{Subject: subject, IssuedAt: now.Unix(), ExpiresAt: now.Add(tokenLifetime).Unix()})
	// [决策理由] Claims 编码失败时不存在可安全签发的载荷。
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature := s.signature(unsigned)

	// >>> 数据演变示例
	// 1. QQ100+时间0 -> header.payload+HMAC -> 三段JWT。
	// 2. QQ200+时间1 -> 不同payload和签名 -> 独立JWT。
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// verify 校验 JWT 结构、HMAC 签名和时间范围。
// @param token：Bearer Token 原文。
// @returns 可信 Claims 或校验错误。
// ⚠️副作用说明：读取当前时间。
func (s *Server) verify(token string) (tokenClaims, error) {
	parts := strings.Split(token, ".")
	// [决策理由] JWT 必须严格包含 header、payload、signature 三段。
	if len(parts) != 3 {
		return tokenClaims{}, errors.New("JWT结构无效")
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[2])
	// [决策理由] 无法解码的签名不能进行可信比较。
	if err != nil || !hmac.Equal(provided, s.signature(parts[0]+"."+parts[1])) {
		return tokenClaims{}, errors.New("JWT签名无效")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	// [决策理由] 必须固定算法为HS256，防止算法替换攻击。
	if err != nil || string(headerBytes) != `{"alg":"HS256","typ":"JWT"}` {
		return tokenClaims{}, errors.New("JWT头无效")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	// [决策理由] Claims 必须可解析并拒绝未知字段。
	if err != nil {
		return tokenClaims{}, errors.New("JWT载荷无效")
	}
	var claims tokenClaims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	// [决策理由] 身份和时间字段缺失时 Token 不能用于授权。
	if err := decoder.Decode(&claims); err != nil || claims.Subject == "" || claims.ExpiresAt == 0 || claims.IssuedAt == 0 {
		return tokenClaims{}, errors.New("JWT声明无效")
	}
	now := s.now().Unix()
	// [决策理由] 已过期 Token 或签发时间明显位于未来都不可信。
	if claims.ExpiresAt <= now || claims.IssuedAt > now+60 {
		return tokenClaims{}, errors.New("JWT已过期或时间无效")
	}

	// >>> 数据演变示例
	// 1. 三段Token+正确HMAC+未过期 -> Claims,nil。
	// 2. payload被修改 -> HMAC不匹配 -> error。
	return claims, nil
}

// signature 计算 JWT 未签名内容的 HMAC-SHA256。
// @param unsigned：JWT 的 header.payload 部分。
// @returns 32字节消息认证码。
// ⚠️副作用说明：无。
func (s *Server) signature(unsigned string) []byte {
	hash := hmac.New(sha256.New, s.jwtSecret)
	_, _ = hash.Write([]byte(unsigned))
	result := hash.Sum(nil)

	// >>> 数据演变示例
	// 1. secretA+payloadA -> HMAC A。
	// 2. secretA+payloadB -> HMAC B。
	return result
}

// middleware 注入请求ID、安全响应头及访问日志。
// @param next：业务路由处理器。
// @returns 包装后的 HTTP 处理器。
// ⚠️副作用说明：读取随机源、修改响应头并写入访问日志。
func (s *Server) middleware(next http.Handler) http.Handler {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		requestID := request.Header.Get("X-Request-ID")
		// [决策理由] 客户端请求ID必须适合审计 VARCHAR(128)，否则改由服务端生成。
		if !validRequestID(requestID) {
			requestID = ""
		}
		// [决策理由] 客户端未提供追踪标识时生成本地随机ID，便于关联审计和故障日志。
		if requestID == "" {
			buffer := make([]byte, 12)
			// [决策理由] 随机源异常时仍使用明确占位符，不影响 API 可用性。
			if _, err := rand.Read(buffer); err != nil {
				requestID = "unavailable"
			} else {
				requestID = hex.EncodeToString(buffer)
			}
		}
		writer.Header().Set("X-Request-ID", requestID)
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		ctx := context.WithValue(request.Context(), requestIDContextKey, requestID)
		next.ServeHTTP(writer, request.WithContext(ctx))
		projectlogger.Info("WebAPI请求", "method", request.Method, "path", request.URL.Path, "request_id", requestID, "duration", time.Since(started))

		// >>> 数据演变示例
		// 1. 无X-Request-ID请求 -> 随机ID+安全头 -> 业务响应+访问日志。
		// 2. X-Request-ID=abc -> 保留abc -> 业务响应+关联日志。
	})

	// >>> 数据演变示例
	// 1. mux -> middleware包装 -> 带安全头处理器。
	// 2. login请求 -> 包装器 -> mux -> login -> 日志。
	return handler
}

// validRequestID 校验客户端追踪标识的长度和安全字符集。
// @param value：X-Request-ID 原文。
// @returns 空值或由字母、数字、点、下划线、冒号、短横线组成且不超过128字符时 true。
// ⚠️副作用说明：无。
func validRequestID(value string) bool {
	// [决策理由] 空值表示需要服务端生成，不属于非法输入。
	if value == "" {
		return true
	}
	// [决策理由] 数据库审计列上限为128，按字节限制可保证写入安全。
	if len(value) > 128 {
		return false
	}
	for _, current := range value {
		allowed := current >= 'a' && current <= 'z' || current >= 'A' && current <= 'Z' || current >= '0' && current <= '9' || current == '.' || current == '_' || current == ':' || current == '-'
		// [决策理由] 拒绝控制字符、空白和日志分隔符，防止审计与日志注入。
		if !allowed {
			return false
		}
	}

	// >>> 数据演变示例
	// 1. req-123:web -> 合法字符且长度内 -> true。
	// 2. 129字符或含换行 -> false并改用服务端ID。
	return true
}

// writeJSON 写入统一 JSON 响应。
// @param writer：响应写入器；status：HTTP 状态码；value：响应对象。
// @returns 无。
// ⚠️副作用说明：设置响应头并写入响应体。
func writeJSON(writer http.ResponseWriter, status int, value responseEnvelope) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	// [决策理由] 响应头已发送后编码错误无法改写状态，仅记录故障供排查。
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		projectlogger.Error("编码WebAPI响应失败", "error", err)
	}

	// >>> 数据演变示例
	// 1. status=200+code=ok -> application/json -> 200统一JSON。
	// 2. status=401+code=unauthorized -> application/json -> 401统一JSON。
}

// writeSuccess 写入统一成功响应。
// @param writer：响应写入器；data：业务响应数据。
// @returns 无。
// ⚠️副作用说明：写入状态码200和 JSON HTTP 响应。
func writeSuccess(writer http.ResponseWriter, data any) {
	writeJSON(writer, http.StatusOK, responseEnvelope{Code: "ok", Message: "操作成功", Data: data})

	// >>> 数据演变示例
	// 1. plugin DTO -> code=ok,message=操作成功,data=plugin。
	// 2. 空列表 -> code=ok,message=操作成功,data=[]。
}

// writeError 写入统一 API 错误结构。
// @param writer：响应写入器；status：HTTP 状态码；code：稳定错误码；message：用户消息。
// @returns 无。
// ⚠️副作用说明：写入 JSON HTTP 响应。
func writeError(writer http.ResponseWriter, status int, code string, message string) {
	writeJSON(writer, status, responseEnvelope{Code: code, Message: message, Data: nil})

	// >>> 数据演变示例
	// 1. 401+unauthorized -> code/message/data:null -> JSON响应。
	// 2. 400+invalid_request -> code/message/data:null -> JSON响应。
}
