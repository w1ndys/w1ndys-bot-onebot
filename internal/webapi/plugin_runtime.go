// 📌 影响范围：读写目标插件架构的全局与群开关；委派鉴权、乐观锁、审计与生命周期。
package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/w1ndys/w1ndys-bot/internal/management"
	"github.com/w1ndys/w1ndys-bot/internal/plugin"
)

// RuntimeStateController 定义目标插件架构对 WebUI 开放的开关与配置能力。
type RuntimeStateController interface {
	List(context.Context, management.Actor) ([]plugin.RuntimeStateView, error)
	Get(context.Context, management.Actor, string) (plugin.RuntimeStateView, error)
	SetGlobalEnabled(context.Context, management.Actor, string, bool, int64) (plugin.RuntimeStateView, error)
	SetGroupEnabled(context.Context, management.Actor, string, int64, bool, int64) (plugin.RuntimeStateView, error)
	PreviewGroupCopy(context.Context, management.Actor, int64, int64) (plugin.RuntimeGroupCopyPreview, error)
	CopyGroupStates(context.Context, management.Actor, int64, int64, []plugin.RuntimeGroupCopyItem) (plugin.RuntimeGroupCopyResult, error)
	PreviewStaleGroupStates(context.Context, management.Actor) ([]plugin.RuntimeStaleGroupState, error)
	CleanupStaleGroupStates(context.Context, management.Actor, []plugin.RuntimeStaleGroupState) ([]plugin.RuntimeStaleGroupState, error)
	GetConfig(context.Context, management.Actor, string) (plugin.RuntimeConfigView, error)
	SetConfig(context.Context, management.Actor, string, json.RawMessage, int64) (plugin.RuntimeConfigView, error)
}

type runtimeConfigWriteRequest struct {
	Config          json.RawMessage `json:"config"`
	ExpectedVersion int64           `json:"expected_version"`
}

type runtimeStateWriteRequest struct {
	Enabled         bool  `json:"enabled"`
	ExpectedVersion int64 `json:"expected_version"`
}

type runtimeGroupCopyPreviewRequest struct {
	SourceGroupID int64 `json:"source_group_id"`
	TargetGroupID int64 `json:"target_group_id"`
}

type runtimeGroupCopyRequest struct {
	SourceGroupID int64                         `json:"source_group_id"`
	TargetGroupID int64                         `json:"target_group_id"`
	States        []runtimeGroupCopyItemRequest `json:"states"`
}

type runtimeGroupCopyItemRequest struct {
	PluginKey     string `json:"plugin_key"`
	Enabled       *bool  `json:"enabled"`
	SourceVersion *int64 `json:"source_version"`
	TargetVersion *int64 `json:"target_version"`
}

type runtimeStaleCleanupItemRequest struct {
	PluginKey       string `json:"plugin_key"`
	GroupID         int64  `json:"group_id"`
	ExpectedVersion int64  `json:"expected_version"`
}

type runtimeStaleCleanupRequest struct {
	States []runtimeStaleCleanupItemRequest `json:"states"`
}

// listPluginRuntimes 返回全部目标插件的意图、运行状态与群开关。
// @param writer：响应写入器；request：已鉴权请求。
// @returns 无。
// ⚠️副作用说明：读取 PostgreSQL 与进程内运行状态并写 JSON 响应。
func (s *Server) listPluginRuntimes(writer http.ResponseWriter, request *http.Request) {
	states, err := s.runtimes.List(request.Context(), actorFromRequest(request))
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeSuccess(writer, states)

	// >>> 数据演变示例
	// 1. echo已启用 -> desired_enabled=true,status=ready -> 200。
	// 2. 未鉴权 -> 401。
}

// getPluginRuntime 返回单个目标插件的意图、运行状态与群开关。
// @param writer：响应写入器；request：携带插件 Key 的已鉴权请求。
// @returns 无。
// ⚠️副作用说明：读取 PostgreSQL 与进程内运行状态并写 JSON 响应。
func (s *Server) getPluginRuntime(writer http.ResponseWriter, request *http.Request) {
	state, err := s.runtimes.Get(request.Context(), actorFromRequest(request), request.PathValue("plugin_key"))
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeSuccess(writer, state)

	// >>> 数据演变示例
	// 1. echo -> 意图+实际状态+群开关 -> 200。
	// 2. 未编译进目录的 Key -> 404。
}

// patchPluginRuntime 按乐观锁修改插件全局启用意图并驱动生命周期。
// @param writer：响应写入器；request：已鉴权严格 JSON 请求。
// @returns 无。
// ⚠️副作用说明：写入状态与审计，并可能执行插件启停生命周期。
func (s *Server) patchPluginRuntime(writer http.ResponseWriter, request *http.Request) {
	var input runtimeStateWriteRequest
	// [决策理由] 全局状态行由目录同步创建，版本必须为正，同时拒绝未知字段。
	if err := decodeJSON(writer, request, &input); err != nil || input.ExpectedVersion <= 0 {
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime", "插件开关参数无效")
		return
	}
	state, err := s.runtimes.SetGlobalEnabled(request.Context(), actorFromRequest(request), request.PathValue("plugin_key"), input.Enabled, input.ExpectedVersion)
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeSuccess(writer, state)

	// >>> 数据演变示例
	// 1. echo,enabled=true,v1 -> 落库v2 -> OnEnable -> status=ready。
	// 2. expected1,current2 -> 409。
}

// putPluginRuntimeGroup 按乐观锁修改插件在单个群的开关。
// @param writer：响应写入器；request：携带插件 Key 与群号的已鉴权请求。
// @returns 无。
// ⚠️副作用说明：写入群状态与审计，并刷新进程内群门禁。
func (s *Server) putPluginRuntimeGroup(writer http.ResponseWriter, request *http.Request) {
	var input runtimeStateWriteRequest
	// [决策理由] version=0 表示尚无群记录，负数和未知字段必须拒绝。
	if err := decodeJSON(writer, request, &input); err != nil || input.ExpectedVersion < 0 {
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime", "插件群开关参数无效")
		return
	}
	groupID, err := strconv.ParseInt(request.PathValue("group_id"), 10, 64)
	// [决策理由] 群号来自可信路径参数，格式错误必须在调用服务前拒绝。
	if err != nil || groupID <= 0 {
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime", "群号无效")
		return
	}
	state, err := s.runtimes.SetGroupEnabled(request.Context(), actorFromRequest(request), request.PathValue("plugin_key"), groupID, input.Enabled, input.ExpectedVersion)
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeSuccess(writer, state)

	// >>> 数据演变示例
	// 1. echo,group100,true,v0 -> 新增v1 -> 该群命令可用。
	// 2. 全局关闭时写入群意图 -> 200，但命令仍被全局门禁拒绝。
}

// previewPluginRuntimeGroupCopy 读取当前群目录并生成源群到目标群的开关差异。
func (s *Server) previewPluginRuntimeGroupCopy(writer http.ResponseWriter, request *http.Request) {
	var input runtimeGroupCopyPreviewRequest
	if err := decodeRuntimeMaintenanceJSON(writer, request, &input, false); err != nil || !validRuntimeCopyGroups(input.SourceGroupID, input.TargetGroupID) {
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime_group_maintenance", "群开关复制参数无效")
		return
	}
	preview, err := s.runtimes.PreviewGroupCopy(request.Context(), actorFromRequest(request), input.SourceGroupID, input.TargetGroupID)
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeSuccess(writer, preview)
}

// copyPluginRuntimeGroups 按预览版本在一个事务内复制源群的显式插件开关。
func (s *Server) copyPluginRuntimeGroups(writer http.ResponseWriter, request *http.Request) {
	var input runtimeGroupCopyRequest
	if err := decodeRuntimeMaintenanceJSON(writer, request, &input, false); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime_group_maintenance", "群开关复制参数无效")
		return
	}
	states, err := validateRuntimeGroupCopyRequest(input)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime_group_maintenance", "群开关复制参数无效")
		return
	}
	result, err := s.runtimes.CopyGroupStates(request.Context(), actorFromRequest(request), input.SourceGroupID, input.TargetGroupID, states)
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeSuccess(writer, result)
}

// previewStalePluginRuntimeGroups 返回数据库中存在、但当前群目录中已不存在的显式门禁记录。
func (s *Server) previewStalePluginRuntimeGroups(writer http.ResponseWriter, request *http.Request) {
	var input struct{}
	if err := decodeRuntimeMaintenanceJSON(writer, request, &input, true); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime_group_maintenance", "失效群开关预览参数无效")
		return
	}
	states, err := s.runtimes.PreviewStaleGroupStates(request.Context(), actorFromRequest(request))
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeSuccess(writer, states)
}

// cleanupStalePluginRuntimeGroups 按预览版本原子删除选中的失效群门禁。
func (s *Server) cleanupStalePluginRuntimeGroups(writer http.ResponseWriter, request *http.Request) {
	var input runtimeStaleCleanupRequest
	if err := decodeRuntimeMaintenanceJSON(writer, request, &input, false); err != nil || validateRuntimeStaleCleanupRequest(input) != nil {
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime_group_maintenance", "失效群开关清理参数无效")
		return
	}
	states := make([]plugin.RuntimeStaleGroupState, 0, len(input.States))
	for _, item := range input.States {
		states = append(states, plugin.RuntimeStaleGroupState{PluginKey: item.PluginKey, GroupID: item.GroupID, Version: item.ExpectedVersion})
	}
	deleted, err := s.runtimes.CleanupStaleGroupStates(request.Context(), actorFromRequest(request), states)
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeSuccess(writer, deleted)
}

func validRuntimeCopyGroups(sourceGroupID, targetGroupID int64) bool {
	return sourceGroupID > 0 && targetGroupID > 0 && sourceGroupID != targetGroupID
}

func validateRuntimeGroupCopyRequest(input runtimeGroupCopyRequest) ([]plugin.RuntimeGroupCopyItem, error) {
	if !validRuntimeCopyGroups(input.SourceGroupID, input.TargetGroupID) || len(input.States) == 0 || len(input.States) > plugin.RuntimeGroupBatchLimit {
		return nil, errors.New("群开关复制数量或群号无效")
	}
	seen := make(map[string]struct{}, len(input.States))
	states := make([]plugin.RuntimeGroupCopyItem, 0, len(input.States))
	for _, item := range input.States {
		key := strings.TrimSpace(item.PluginKey)
		if key == "" || key != item.PluginKey || item.Enabled == nil || item.SourceVersion == nil || *item.SourceVersion <= 0 || item.TargetVersion == nil || *item.TargetVersion < 0 {
			return nil, errors.New("群开关复制项无效")
		}
		if _, exists := seen[key]; exists {
			return nil, plugin.ErrRuntimeGroupStateDuplicate
		}
		seen[key] = struct{}{}
		states = append(states, plugin.RuntimeGroupCopyItem{PluginKey: key, Enabled: *item.Enabled, SourceVersion: *item.SourceVersion, TargetVersion: *item.TargetVersion})
	}
	return states, nil
}

func validateRuntimeStaleCleanupRequest(input runtimeStaleCleanupRequest) error {
	if len(input.States) == 0 || len(input.States) > plugin.RuntimeGroupBatchLimit {
		return errors.New("失效群开关清理数量无效")
	}
	seen := make(map[string]struct{}, len(input.States))
	for _, item := range input.States {
		pluginKey := strings.TrimSpace(item.PluginKey)
		if pluginKey == "" || pluginKey != item.PluginKey || item.GroupID <= 0 || item.ExpectedVersion <= 0 {
			return errors.New("失效群开关清理项无效")
		}
		key := fmt.Sprintf("%s\x00%d", pluginKey, item.GroupID)
		if _, exists := seen[key]; exists {
			return plugin.ErrRuntimeGroupStateDuplicate
		}
		seen[key] = struct{}{}
	}
	return nil
}

// decodeRuntimeMaintenanceJSON 为最多一千条门禁记录提供严格且有界的 JSON 解码。
func decodeRuntimeMaintenanceJSON(writer http.ResponseWriter, request *http.Request, target any, allowEmpty bool) error {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 256<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("请求体只能包含一个 JSON 值")
		}
		return err
	}
	return nil
}

// getPluginRuntimeConfig 返回插件配置 Schema 与脱敏后的当前值。
// @param writer：响应写入器；request：携带插件 Key 的已鉴权请求。
// @returns 无。
// ⚠️副作用说明：读取 PostgreSQL 并写 JSON 响应。
func (s *Server) getPluginRuntimeConfig(writer http.ResponseWriter, request *http.Request) {
	view, err := s.runtimes.GetConfig(request.Context(), actorFromRequest(request), request.PathValue("plugin_key"))
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeSuccess(writer, view)

	// >>> 数据演变示例
	// 1. echo -> Schema+脱敏值+版本 -> 200。
	// 2. 未声明配置的插件 -> 404。
}

// putPluginRuntimeConfig 按乐观锁保存插件配置并热应用。
// @param writer：响应写入器；request：已鉴权严格 JSON 请求。
// @returns 无。
// ⚠️副作用说明：写入配置与审计，并调用插件热应用钩子。
func (s *Server) putPluginRuntimeConfig(writer http.ResponseWriter, request *http.Request) {
	var input runtimeConfigWriteRequest
	// [决策理由] 配置行随目录同步预建，版本必须为正；未知字段与缺失配置对象一律拒绝。
	if err := decodeJSON(writer, request, &input); err != nil || input.ExpectedVersion <= 0 || len(input.Config) == 0 {
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime_config", "插件配置参数无效")
		return
	}
	view, err := s.runtimes.SetConfig(request.Context(), actorFromRequest(request), request.PathValue("plugin_key"), input.Config, input.ExpectedVersion)
	if err != nil {
		writeRuntimeError(writer, err)
		return
	}
	writeSuccess(writer, view)

	// >>> 数据演变示例
	// 1. echo+{response_prefix:"[bot] "}+v1 -> 保存并热应用 -> v2。
	// 2. 陈旧版本 -> 409。
}

// writeRuntimeError 将目标插件开关的领域错误映射为稳定 HTTP 响应。
// @param writer：响应写入器；err：服务层错误。
// @returns 无。
// ⚠️副作用说明：写入 JSON 错误响应；未识别错误记录服务端日志。
func writeRuntimeError(writer http.ResponseWriter, err error) {
	switch {
	// [决策理由] 未编译进目录或尚未同步状态行的插件不是可管理资源。
	case errors.Is(err, plugin.ErrRuntimePluginNotFound), errors.Is(err, plugin.ErrRuntimeStateNotFound):
		writeError(writer, http.StatusNotFound, "plugin_runtime_not_found", "插件不存在")
	case errors.Is(err, plugin.ErrRuntimeGroupSourceStateNotFound):
		writeError(writer, http.StatusNotFound, "plugin_runtime_group_source_state_not_found", "源群没有可复制的显式插件开关")
	// [决策理由] 陈旧版本要求前端重新读取后再提交。
	case errors.Is(err, plugin.ErrRuntimeStateConflict):
		writeError(writer, http.StatusConflict, "plugin_runtime_conflict", "插件开关已被其他操作更新")
	case errors.Is(err, plugin.ErrRuntimeGroupNotLive):
		writeError(writer, http.StatusConflict, "plugin_runtime_group_not_live", "群已不在机器人当前群列表中，请重新预览")
	case errors.Is(err, plugin.ErrRuntimeGroupStateNotStale):
		writeError(writer, http.StatusConflict, "plugin_runtime_group_not_stale", "群已重新出现，请重新预览")
	case errors.Is(err, plugin.ErrRuntimeGroupDirectoryUnavailable):
		writeError(writer, http.StatusServiceUnavailable, "plugin_runtime_group_directory_unavailable", "无法读取机器人当前群列表，请稍后重试")
	case errors.Is(err, plugin.ErrRuntimeStateCommitUnknown):
		writeError(writer, http.StatusServiceUnavailable, "plugin_runtime_commit_unknown", "数据库提交结果暂时无法确认，相关群开关已保持关闭，请刷新后重新预览")
	// [决策理由] 正在启停的插件必须等待本次切换结束，属于可重试冲突。
	case errors.Is(err, plugin.ErrRuntimeTransition):
		writeError(writer, http.StatusConflict, "plugin_runtime_transition", "插件正在切换运行状态")
	// [决策理由] 故障插件必须先关闭完成清理才能重新启用。
	case errors.Is(err, plugin.ErrRuntimeRecoveryNeeded):
		writeError(writer, http.StatusConflict, "plugin_runtime_recovery_needed", "插件故障后必须先禁用再启用")
	// [决策理由] 群号非法属于可修正输入。
	case errors.Is(err, plugin.ErrInvalidRuntimeGroupID):
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime", "群号无效")
	case errors.Is(err, plugin.ErrRuntimeGroupCopySameGroup), errors.Is(err, plugin.ErrRuntimeGroupBatchEmpty), errors.Is(err, plugin.ErrRuntimeGroupBatchTooLarge), errors.Is(err, plugin.ErrRuntimeGroupStateDuplicate), errors.Is(err, plugin.ErrRuntimeStateInvalidVersion):
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime_group_maintenance", "群开关维护参数无效")
	// [决策理由] 未声明配置的插件没有该子资源，不应伪装成空 Schema。
	case errors.Is(err, plugin.ErrRuntimeConfigNotSupported), errors.Is(err, plugin.ErrRuntimeConfigNotFound):
		writeError(writer, http.StatusNotFound, "plugin_runtime_config_not_supported", "插件未声明可管理配置")
	// [决策理由] Schema 或插件领域校验失败属于客户端配置错误。
	case errors.Is(err, plugin.ErrRuntimeConfigInvalid):
		writeError(writer, http.StatusBadRequest, "invalid_plugin_runtime_config", "插件配置无效")
	default:
		// [决策理由] 授权失败与未识别错误复用统一管理错误映射，避免两套响应语义。
		writeManagementError(writer, err)
	}

	// >>> 数据演变示例
	// 1. ErrRuntimeStateConflict -> 409 plugin_runtime_conflict。
	// 2. admin.ErrForbidden -> 交给 writeManagementError -> 403。
}
