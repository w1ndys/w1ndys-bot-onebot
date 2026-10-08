// 📌 影响范围：读写违禁监控的复核队列、文本试判与加权词库；写操作会修改数据库、审计，并可能调用 NapCat 与外部模型。
package webapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/w1ndys/w1ndys-bot/internal/management"
	forbiddenmonitor "github.com/w1ndys/w1ndys-bot/plugins/forbidden_message_monitor"
)

// ForbiddenMonitorController 定义违禁监控插件对 WebUI 开放的管理能力。
type ForbiddenMonitorController interface {
	ListViolations(context.Context, management.Actor, int, int) (forbiddenmonitor.RecordPage, error)
	ReviewViolation(context.Context, management.Actor, int64, int64, string) (forbiddenmonitor.Record, error)
	RunTextTrial(context.Context, management.Actor, string) (forbiddenmonitor.Record, error)
	ListTerms(context.Context, management.Actor, int, int) ([]forbiddenmonitor.Term, int64, error)
	CreateTerm(context.Context, management.Actor, forbiddenmonitor.TermInput) (forbiddenmonitor.Term, error)
	UpdateTerm(context.Context, management.Actor, int64, int64, forbiddenmonitor.TermInput) (forbiddenmonitor.Term, error)
	DeleteTerm(context.Context, management.Actor, int64, int64) error
}

type reviewRequest struct {
	Status          string `json:"status"`
	ExpectedVersion int64  `json:"expected_version"`
}

type textTrialRequest struct {
	Text string `json:"text"`
}

type createTermRequest struct {
	Text   string  `json:"text"`
	Weight float64 `json:"weight"`
}

type updateTermRequest struct {
	Text            string  `json:"text"`
	Weight          float64 `json:"weight"`
	ExpectedVersion int64   `json:"expected_version"`
}

type versionedDeleteRequest struct {
	ExpectedVersion int64 `json:"expected_version"`
}

// listViolations 分页返回待人工复核的违规记录。
// @param writer：响应写入器；request：已鉴权请求。
// @returns 无。
// ⚠️副作用说明：读取 PostgreSQL 并写 JSON 响应。
func (s *Server) listViolations(writer http.ResponseWriter, request *http.Request) {
	page, pageSize, ok := forbiddenPagination(writer, request)
	if !ok {
		return
	}
	result, err := s.forbiddenMonitor.ListViolations(request.Context(), actorFromRequest(request), page, pageSize)
	if err != nil {
		writeForbiddenMonitorError(writer, err)
		return
	}
	writeSuccess(writer, result)

	// >>> 数据演变示例
	// 1. page1 -> 待复核分页 -> 200。
	// 2. 未鉴权 -> 401。
}

// reviewViolation 按乐观锁提交人工复核结论。
// @param writer：响应写入器；request：携带记录 ID 的已鉴权请求。
// @returns 无。
// ⚠️副作用说明：写入复核结论与审计；标记误报时可能解除该记录对应的自动禁言。
func (s *Server) reviewViolation(writer http.ResponseWriter, request *http.Request) {
	id, ok := pathInt64(writer, request, "violation_id", "invalid_forbidden_monitor")
	if !ok {
		return
	}
	var input reviewRequest
	if err := decodeJSON(writer, request, &input); err != nil || input.ExpectedVersion <= 0 || input.Status == "" {
		writeError(writer, http.StatusBadRequest, "invalid_forbidden_monitor", "复核参数无效")
		return
	}
	record, err := s.forbiddenMonitor.ReviewViolation(request.Context(), actorFromRequest(request), id, input.ExpectedVersion, input.Status)
	if err != nil {
		writeForbiddenMonitorError(writer, err)
		return
	}
	writeSuccess(writer, record)

	// >>> 数据演变示例
	// 1. id1+确认+v1 -> 处置并落审计 -> 200。
	// 2. 陈旧版本 -> 409。
}

// createTextTrial 使用当前规则试判文本。
// @param writer：响应写入器；request：已鉴权严格 JSON 请求。
// @returns 无。
// ⚠️副作用说明：可能消耗大模型额度；不禁言、不撤回，也不写入违规审计。
func (s *Server) createTextTrial(writer http.ResponseWriter, request *http.Request) {
	var input textTrialRequest
	if err := decodeJSON(writer, request, &input); err != nil || input.Text == "" {
		writeError(writer, http.StatusBadRequest, "invalid_forbidden_monitor", "试判文本无效")
		return
	}
	record, err := s.forbiddenMonitor.RunTextTrial(request.Context(), actorFromRequest(request), input.Text)
	if err != nil {
		writeForbiddenMonitorError(writer, err)
		return
	}
	writeSuccess(writer, record)

	// >>> 数据演变示例
	// 1. 高权重文本 -> weighted_score 违规结论 -> 200。
	// 2. 空文本 -> 400。
}

// listTerms 分页返回加权违禁词。
// @param writer：响应写入器；request：已鉴权请求。
// @returns 无。
// ⚠️副作用说明：读取 PostgreSQL 并写 JSON 响应。
func (s *Server) listTerms(writer http.ResponseWriter, request *http.Request) {
	page, pageSize, ok := forbiddenPagination(writer, request)
	if !ok {
		return
	}
	items, total, err := s.forbiddenMonitor.ListTerms(request.Context(), actorFromRequest(request), page, pageSize)
	if err != nil {
		writeForbiddenMonitorError(writer, err)
		return
	}
	writeSuccess(writer, map[string]any{"items": items, "page": page, "page_size": pageSize, "total": total})

	// >>> 数据演变示例
	// 1. page1 -> 加权违禁词分页 -> 200。
	// 2. page_size超限 -> 400。
}

// createTerm 新增词库词条并重建检测引擎。
// @param writer：响应写入器；request：已鉴权严格 JSON 请求。
// @returns 无。
// ⚠️副作用说明：写入词条与审计，并按新词库重建检测引擎。
func (s *Server) createTerm(writer http.ResponseWriter, request *http.Request) {
	var input createTermRequest
	if err := decodeJSON(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_forbidden_monitor", "词条参数无效")
		return
	}
	term, err := s.forbiddenMonitor.CreateTerm(request.Context(), actorFromRequest(request), forbiddenmonitor.TermInput{Text: input.Text, Weight: input.Weight})
	if err != nil {
		writeForbiddenMonitorError(writer, err)
		return
	}
	writeSuccess(writer, term)

	// >>> 数据演变示例
	// 1. "加微信"+25 -> 新增 v1 并热重建引擎 -> 200。
	// 2. 清洗后重复词条 -> 409。
}

// updateTerm 按乐观锁更新词库词条并重建检测引擎。
// @param writer：响应写入器；request：携带词条 ID 的已鉴权请求。
// @returns 无。
// ⚠️副作用说明：写入词条与审计，并按新词库重建检测引擎。
func (s *Server) updateTerm(writer http.ResponseWriter, request *http.Request) {
	id, ok := pathInt64(writer, request, "term_id", "invalid_forbidden_monitor")
	if !ok {
		return
	}
	var input updateTermRequest
	if err := decodeJSON(writer, request, &input); err != nil || input.ExpectedVersion <= 0 {
		writeError(writer, http.StatusBadRequest, "invalid_forbidden_monitor", "词条参数无效")
		return
	}
	term, err := s.forbiddenMonitor.UpdateTerm(request.Context(), actorFromRequest(request), id, input.ExpectedVersion, forbiddenmonitor.TermInput{Text: input.Text, Weight: input.Weight})
	if err != nil {
		writeForbiddenMonitorError(writer, err)
		return
	}
	writeSuccess(writer, term)

	// >>> 数据演变示例
	// 1. id1+v1 -> 更新权重 -> v2。
	// 2. 陈旧版本 -> 409。
}

// deleteTerm 按乐观锁删除词库词条并重建检测引擎。
// @param writer：响应写入器；request：携带词条 ID 的已鉴权请求。
// @returns 无。
// ⚠️副作用说明：删除词条、写审计并按新词库重建检测引擎。
func (s *Server) deleteTerm(writer http.ResponseWriter, request *http.Request) {
	id, ok := pathInt64(writer, request, "term_id", "invalid_forbidden_monitor")
	if !ok {
		return
	}
	version, ok := decodeVersion(writer, request)
	if !ok {
		return
	}
	if err := s.forbiddenMonitor.DeleteTerm(request.Context(), actorFromRequest(request), id, version); err != nil {
		writeForbiddenMonitorError(writer, err)
		return
	}
	writeSuccess(writer, map[string]bool{"deleted": true})

	// >>> 数据演变示例
	// 1. id1+v2 -> 删除并热重建引擎 -> 200。
	// 2. 词条已被删除 -> 404。
}

func forbiddenPagination(writer http.ResponseWriter, request *http.Request) (int, int, bool) {
	page, pageSize := 1, 20
	var err error
	for name, values := range request.URL.Query() {
		if (name != "page" && name != "page_size") || len(values) != 1 || values[0] == "" {
			writeError(writer, http.StatusBadRequest, "invalid_forbidden_monitor", "分页参数无效")
			return 0, 0, false
		}
	}
	if raw := request.URL.Query().Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 1_000_000 {
			writeError(writer, http.StatusBadRequest, "invalid_forbidden_monitor", "分页参数无效")
			return 0, 0, false
		}
	}
	if raw := request.URL.Query().Get("page_size"); raw != "" {
		pageSize, err = strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > 200 {
			writeError(writer, http.StatusBadRequest, "invalid_forbidden_monitor", "分页参数无效")
			return 0, 0, false
		}
	}
	return page, pageSize, true
}

// pathInt64 解析路径中的正整数标识。
// @param writer：响应写入器；request：当前请求；name：路径参数名；code：错误码。
// @returns 正整数标识及是否有效；无效时已写出 400。
// ⚠️副作用说明：无效时写入 JSON 错误响应。
func pathInt64(writer http.ResponseWriter, request *http.Request, name, code string) (int64, bool) {
	value, err := strconv.ParseInt(request.PathValue(name), 10, 64)
	if err != nil || value <= 0 {
		writeError(writer, http.StatusBadRequest, code, "记录标识无效")
		return 0, false
	}

	// >>> 数据演变示例
	// 1. "8" -> 8,true。
	// 2. "abc" -> 400,false。
	return value, true
}

// decodeVersion 解析仅携带乐观锁版本的请求体。
// @param writer：响应写入器；request：当前请求。
// @returns 正整数版本及是否有效；无效时已写出 400。
// ⚠️副作用说明：读取请求体；无效时写入 JSON 错误响应。
func decodeVersion(writer http.ResponseWriter, request *http.Request) (int64, bool) {
	var input versionedDeleteRequest
	if err := decodeJSON(writer, request, &input); err != nil || input.ExpectedVersion <= 0 {
		writeError(writer, http.StatusBadRequest, "invalid_forbidden_monitor", "缺少有效的乐观锁版本")
		return 0, false
	}

	// >>> 数据演变示例
	// 1. {"expected_version":2} -> 2,true。
	// 2. {} -> 400,false。
	return input.ExpectedVersion, true
}

// writeForbiddenMonitorError 将违禁监控领域错误映射为稳定 HTTP 响应。
// @param writer：响应写入器；err：服务层错误。
// @returns 无。
// ⚠️副作用说明：写入 JSON 错误响应；未识别错误记录服务端日志。
func writeForbiddenMonitorError(writer http.ResponseWriter, err error) {
	switch {
	// [决策理由] 记录被并发删除时前端应刷新当前页。
	case errors.Is(err, forbiddenmonitor.ErrLexiconNotFound), errors.Is(err, management.ErrResourceRecordNotFound):
		writeError(writer, http.StatusNotFound, "forbidden_monitor_not_found", "违禁监控记录不存在")
	// [决策理由] 词条重复与陈旧版本都要求前端刷新后重试。
	case errors.Is(err, forbiddenmonitor.ErrLexiconConflict), errors.Is(err, management.ErrResourceConflict):
		writeError(writer, http.StatusConflict, "forbidden_monitor_conflict", "违禁监控记录已被其他操作更新或重复")
	case errors.Is(err, forbiddenmonitor.ErrRuntimeUnavailable):
		writeError(writer, http.StatusConflict, "forbidden_monitor_runtime_unavailable", "违禁消息监控或目标群当前未启用")
	// [决策理由] 字段与分页边界错误属于可修正输入。
	case errors.Is(err, forbiddenmonitor.ErrInvalidInput), errors.Is(err, forbiddenmonitor.ErrInvalidLexicon), errors.Is(err, management.ErrInvalidResourceData):
		writeError(writer, http.StatusBadRequest, "invalid_forbidden_monitor", "违禁监控参数无效")
	default:
		// [决策理由] 授权失败与未识别错误复用统一管理错误映射，避免两套响应语义。
		writeManagementError(writer, err)
	}

	// >>> 数据演变示例
	// 1. ErrLexiconConflict -> 409 forbidden_monitor_conflict。
	// 2. admin.ErrForbidden -> 交给 writeManagementError -> 403。
}
