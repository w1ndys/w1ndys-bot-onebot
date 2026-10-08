// 📌 影响范围：读写 PostgreSQL 违禁监控业务表与管理审计日志；不调用外部服务。
package forbiddenmessagemonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/w1ndys/w1ndys-bot/internal/management"
	"github.com/w1ndys/w1ndys-bot/internal/onebot"
	"github.com/w1ndys/w1ndys-bot/internal/plugin"
)

const (
	statusPendingReview        = "pending_review"
	statusConfirmedPendingKick = "confirmed_pending_kick"
	statusConfirmedKicked      = "confirmed_kicked"
	statusFalsePositivePending = "false_positive_unban_pending"
	statusFalsePositive        = "false_positive_unbanned"
)

type monitorRepository interface {
	IncrementValidSpeech(context.Context, int64, int64, time.Time) error
	ListObservedGroups(context.Context) ([]int64, error)
	RecentValidCounts(context.Context, int64, time.Time) (map[int64]int, error)
	IsWhitelisted(context.Context, int64, int64) (bool, error)
	RemoveWhitelist(context.Context, int64, int64) error
	ReplaceWhitelist(context.Context, int64, []int64, time.Time) error
	CreateViolation(context.Context, violationCreate) (int64, error)
	ReserveViolation(context.Context, violationCreate) (int64, bool, error)
	CompleteViolationActions(context.Context, int64, moderationOutcome) error
	GetViolation(context.Context, int64) (storedViolation, error)
	TransitionByEvent(context.Context, management.Actor, int64, int64, time.Time, time.Time, string) (bool, error)
	TryReserveLLMRequest(context.Context, time.Time, int) (bool, error)
	ListPending(context.Context, management.ResourceQuery) (management.ResourcePage, error)
	Review(context.Context, management.Actor, int64, int64, string) (management.ResourceRecord, error)
	BeginFalsePositive(context.Context, int64, int64) (storedViolation, error)
	FinishFalsePositive(context.Context, management.Actor, int64, int64) (management.ResourceRecord, error)
	CancelFalsePositive(context.Context, int64, int64) error
}

type postgresMonitorRepository struct{ pool *pgxpool.Pool }

type violationCreate struct {
	MessageID       *int64
	MessageContent  string
	GroupID         int64
	UserID          int64
	DetectionSource string
	RiskScore       *float64
	Reason          string
	Violations      []string
	ActionResult    json.RawMessage
	MessageTime     time.Time
}

type violationData struct {
	MessageID       *int64          `json:"message_id,omitempty"`
	MessageContent  string          `json:"msg_content"`
	GroupID         int64           `json:"group_id"`
	UserID          int64           `json:"user_id"`
	Status          string          `json:"status"`
	DetectionSource string          `json:"detection_source"`
	RiskScore       *float64        `json:"risk_score,omitempty"`
	Reason          string          `json:"reason"`
	Violations      json.RawMessage `json:"violations"`
	ActionResult    json.RawMessage `json:"action_result"`
	MessageTime     time.Time       `json:"message_time"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type storedViolation struct {
	ID      int64
	Version int64
	Data    violationData
}

type reviewPayload struct {
	Status string `json:"status"`
}

// TryReserveLLMRequest 原子预占UTC日期内的一次模型请求额度。
// @param ctx：查询上下文；at：请求时间；dailyLimit：每日最大请求数。
// @returns 预占成功为true，额度耗尽为false，或数据库错误。
// ⚠️副作用说明：成功时递增当日持久化请求计数。
func (r *postgresMonitorRepository) TryReserveLLMRequest(ctx context.Context, at time.Time, dailyLimit int) (bool, error) {
	var count int
	err := r.pool.QueryRow(ctx, `INSERT INTO forbidden_monitor_llm_usage_daily(usage_date,request_count) VALUES($1,1) ON CONFLICT(usage_date) DO UPDATE SET request_count=forbidden_monitor_llm_usage_daily.request_count+1,updated_at=NOW(),version=forbidden_monitor_llm_usage_daily.version+1 WHERE forbidden_monitor_llm_usage_daily.request_count<$2 RETURNING request_count`, at.UTC().Format(time.DateOnly), dailyLimit).Scan(&count)
	// [决策理由] 冲突更新条件未满足时无返回行，表示当日额度已用尽而非数据库故障。
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	// [决策理由] 其他错误必须阻止外部调用，避免预算控制失效。
	if err != nil {
		return false, err
	}

	// >>> 数据演变示例
	// 1. limit500且当前499 -> 递增500 -> true。
	// 2. limit500且当前500 -> WHERE拒绝更新 -> false。
	return true, nil
}

type violationResourceHandler struct {
	repository monitorRepository
	actions    plugin.ActionAPI
}

// newPostgresMonitorRepository 创建插件自有仓储。
// @param pool：应用共享PostgreSQL连接池。
// @returns 违禁监控仓储。
// ⚠️副作用说明：仅保存连接池引用。
func newPostgresMonitorRepository(pool *pgxpool.Pool) *postgresMonitorRepository {
	result := &postgresMonitorRepository{pool: pool}
	// >>> 数据演变示例
	// 1. pool -> repository{pool}。
	// 2. nil pool -> repository{nil}，调用时由注入边界保证。
	return result
}

// IncrementValidSpeech 按群、用户与UTC日期原子累加有效发言。
// @param ctx：查询上下文；groupID/userID：群与用户；at：发言时间。
// @returns 数据库错误。
// ⚠️副作用说明：插入或更新每日发言计数并递增版本。
func (r *postgresMonitorRepository) IncrementValidSpeech(ctx context.Context, groupID, userID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO forbidden_monitor_daily_speech_counts(group_id,user_id,speech_date,valid_count) VALUES($1,$2,$3,1) ON CONFLICT(group_id,user_id,speech_date) DO UPDATE SET valid_count=forbidden_monitor_daily_speech_counts.valid_count+1,version=forbidden_monitor_daily_speech_counts.version+1,updated_at=NOW()`, groupID, userID, at.UTC().Format(time.DateOnly))
	// >>> 数据演变示例
	// 1. 当日无记录 -> count1,version1。
	// 2. 当日count2 -> count3,version+1。
	return err
}

// ListObservedGroups 返回插件业务表中出现过的群集合。
// @param ctx：查询上下文。
// @returns 升序去重群号或数据库错误。
// ⚠️副作用说明：执行一次只读联合查询。
func (r *postgresMonitorRepository) ListObservedGroups(ctx context.Context) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `SELECT group_id FROM forbidden_monitor_daily_speech_counts UNION SELECT group_id FROM forbidden_monitor_whitelist UNION SELECT group_id FROM forbidden_monitor_violation_audits ORDER BY group_id`)
	// [决策理由] 无法建立完整群集合时不得遗漏某群的每日白名单刷新。
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]int64, 0)
	for rows.Next() {
		var groupID int64
		// [决策理由] 任一群号扫描失败会使刷新集合不完整，必须整体失败。
		if err := rows.Scan(&groupID); err != nil {
			return nil, err
		}
		result = append(result, groupID)
	}
	// [决策理由] 迭代错误只有Rows.Err能完整报告。
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// >>> 数据演变示例
	// 1. 计数群1+审计群2 -> [1,2]。
	// 2. 三表为空 -> []。
	return result, nil
}

// RecentValidCounts 聚合指定日期起每位用户的有效发言数。
// @param ctx：查询上下文；groupID：目标群；since：包含边界起始时刻，其UTC日期用于日表查询。
// @returns userID到有效发言总数的映射或数据库错误。
// ⚠️副作用说明：执行一次只读聚合查询。
func (r *postgresMonitorRepository) RecentValidCounts(ctx context.Context, groupID int64, since time.Time) (map[int64]int, error) {
	rows, err := r.pool.Query(ctx, `SELECT user_id,SUM(valid_count)::BIGINT FROM forbidden_monitor_daily_speech_counts WHERE group_id=$1 AND speech_date >= $2 GROUP BY user_id`, groupID, since.UTC().Format(time.DateOnly))
	// [决策理由] 聚合查询失败时不能用空计数误删全部白名单。
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]int)
	for rows.Next() {
		var userID int64
		var count int64
		// [决策理由] 任一用户计数不可信时不得发布部分白名单。
		if err := rows.Scan(&userID, &count); err != nil {
			return nil, err
		}
		result[userID] = int(count)
	}
	// [决策理由] 连接中断等迭代错误必须在返回聚合前检查。
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// >>> 数据演变示例
	// 1. 用户7两日1+2 -> {7:3}。
	// 2. 窗口内无发言 -> 空map。
	return result, nil
}

// IsWhitelisted 检查用户是否在群白名单。
// @param ctx：查询上下文；groupID/userID：群与用户。
// @returns 是否命中与数据库错误。
// ⚠️副作用说明：执行一次只读查询。
func (r *postgresMonitorRepository) IsWhitelisted(ctx context.Context, groupID, userID int64) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM forbidden_monitor_whitelist WHERE group_id=$1 AND user_id=$2)`, groupID, userID).Scan(&exists)
	// >>> 数据演变示例
	// 1. (100,200)存在 -> true,nil。
	// 2. 不存在 -> false,nil。
	return exists, err
}

// RemoveWhitelist 在成员入群时立即移除可能残留的旧白名单。
// @param ctx：查询上下文；groupID/userID：新入群成员定位键。
// @returns 数据库错误。
// ⚠️副作用说明：删除对应白名单行；不存在时幂等成功。
func (r *postgresMonitorRepository) RemoveWhitelist(ctx context.Context, groupID, userID int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM forbidden_monitor_whitelist WHERE group_id=$1 AND user_id=$2`, groupID, userID)

	// >>> 数据演变示例
	// 1. 旧白名单存在+重新入群 -> 删除 -> 首条消息过检。
	// 2. 新用户无旧记录 -> 删除0行 -> nil。
	return err
}

// ReplaceWhitelist 在单个事务中替换指定群白名单。
// @param ctx：事务上下文；groupID：群；userIDs：刷新后用户；refreshedAt：刷新时间。
// @returns 事务错误。
// ⚠️副作用说明：锁定该群白名单、批量替换并提交。
func (r *postgresMonitorRepository) ReplaceWhitelist(ctx context.Context, groupID int64, userIDs []int64, refreshedAt time.Time) error {
	tx, err := r.pool.Begin(ctx)
	// [决策理由] 删除与重建必须原子可见。
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// [决策理由] 事务级咨询锁按群串行化并发刷新，包括当前空表的情况。
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, groupID); err != nil {
		return err
	}
	uniqueUserIDs := make([]int64, 0, len(userIDs))
	seen := make(map[int64]struct{}, len(userIDs))
	for _, userID := range userIDs {
		// [决策理由] 同一成员重复出现时只保留一个，避免批量UPSERT重复影响同一行。
		if _, exists := seen[userID]; !exists {
			seen[userID] = struct{}{}
			uniqueUserIDs = append(uniqueUserIDs, userID)
		}
	}
	// [决策理由] UPSERT只刷新持续满足成员并保留added_at，新增成员才生成首次加入时间。
	if _, err := tx.Exec(ctx, `INSERT INTO forbidden_monitor_whitelist(group_id,user_id,refreshed_at) SELECT $1,user_id,$3 FROM unnest($2::BIGINT[]) AS user_id ON CONFLICT(group_id,user_id) DO UPDATE SET refreshed_at=EXCLUDED.refreshed_at,version=forbidden_monitor_whitelist.version+1`, groupID, uniqueUserIDs, refreshedAt); err != nil {
		return err
	}
	// [决策理由] 新集合发布前只移除不再满足的旧成员，确保持续成员的首次加入时间可审计。
	if _, err := tx.Exec(ctx, `DELETE FROM forbidden_monitor_whitelist WHERE group_id=$1 AND NOT (user_id=ANY($2::BIGINT[]))`, groupID, uniqueUserIDs); err != nil {
		return err
	}
	// [决策理由] 仅完整新集合可对检测流量可见。
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// >>> 数据演变示例
	// 1. 旧[1,2]+新[2,3] -> 事务后[2,3]。
	// 2. 新[] -> 事务后该群空集合。
	return nil
}

// CreateViolation 持久化一条待人工复核记录。
// @param ctx：查询上下文；input：检测证据和处置结果。
// @returns 新记录ID或数据库/序列化错误。
// ⚠️副作用说明：插入违规审计表。
func (r *postgresMonitorRepository) CreateViolation(ctx context.Context, input violationCreate) (int64, error) {
	violations, err := json.Marshal(input.Violations)
	// [决策理由] 不完整敏感词证据不得落库。
	if err != nil {
		return 0, err
	}
	actionResult := input.ActionResult
	// [决策理由] 空处置结果应规范为JSON对象，满足表约束。
	if len(actionResult) == 0 {
		actionResult = json.RawMessage(`{}`)
	}
	var id int64
	err = r.pool.QueryRow(ctx, `INSERT INTO forbidden_monitor_violation_audits(message_id,msg_content,group_id,user_id,detection_source,risk_score,reason,violations,action_result,message_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, input.MessageID, input.MessageContent, input.GroupID, input.UserID, input.DetectionSource, input.RiskScore, input.Reason, violations, actionResult, input.MessageTime).Scan(&id)
	// >>> 数据演变示例
	// 1. 规则命中+禁言成功 -> pending_review,id。
	// 2. 同群同message_id -> 唯一约束错误。
	return id, err
}

// ReserveViolation 在执行外部处罚前以消息唯一键预留审计记录。
// @param ctx：查询上下文；input：不可变检测证据，message_id必须有效。
// @returns 记录ID、是否由本次调用成功预留，以及数据库错误。
// ⚠️副作用说明：成功时插入一条处置结果为空的待复核审计；重复消息不修改数据。
func (r *postgresMonitorRepository) ReserveViolation(ctx context.Context, input violationCreate) (int64, bool, error) {
	// [决策理由] 自动处罚必须有稳定消息键才能保证重复投递不会重复执行外部动作。
	if input.MessageID == nil || *input.MessageID <= 0 {
		return 0, false, management.ErrInvalidResourceData
	}
	violations, err := json.Marshal(input.Violations)
	// [决策理由] 检测证据无法编码时不得预留不完整审计或继续处罚。
	if err != nil {
		return 0, false, err
	}
	var id int64
	err = r.pool.QueryRow(ctx, `INSERT INTO forbidden_monitor_violation_audits(message_id,msg_content,group_id,user_id,detection_source,risk_score,reason,violations,action_result,message_time) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'{}'::jsonb,$9) ON CONFLICT(group_id,message_id) DO NOTHING RETURNING id`, input.MessageID, input.MessageContent, input.GroupID, input.UserID, input.DetectionSource, input.RiskScore, input.Reason, violations, input.MessageTime).Scan(&id)
	// [决策理由] 无返回行表示另一事件已预留或完成同一消息，应视为幂等成功且禁止重复处罚。
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	// [决策理由] 其他数据库错误必须阻止任何外部处罚。
	if err != nil {
		return 0, false, err
	}
	// >>> 数据演变示例
	// 1. message9首次到达 -> 插入id7 -> {7,true,nil}。
	// 2. message9重复到达 -> ON CONFLICT无行 -> {0,false,nil}。
	return id, true, nil
}

// CompleteViolationActions 写回已预留违规记录的处置摘要。
// @param ctx：查询上下文；id：预留记录ID；outcome：各外部动作的脱敏结果。
// @returns 编码、记录不存在或数据库错误。
// ⚠️副作用说明：更新指定审计记录的action_result、版本和更新时间。
func (r *postgresMonitorRepository) CompleteViolationActions(ctx context.Context, id int64, outcome moderationOutcome) error {
	actionResult, err := json.Marshal(outcome)
	// [决策理由] 动作摘要必须保持合法JSON对象才能满足审计表约束。
	if err != nil {
		return err
	}
	command, err := r.pool.Exec(ctx, `UPDATE forbidden_monitor_violation_audits SET action_result=$1,version=version+1,updated_at=NOW() WHERE id=$2`, actionResult, id)
	// [决策理由] 数据库错误必须暴露给事件链路，不能伪装成已完成审计。
	if err != nil {
		return err
	}
	// [决策理由] 预留记录意外消失意味着处置结果无法追溯，必须返回明确错误。
	if command.RowsAffected() != 1 {
		return management.ErrResourceRecordNotFound
	}
	// >>> 数据演变示例
	// 1. id7+禁言成功撤回2条 -> action_result更新且version+1。
	// 2. id不存在 -> RowsAffected=0 -> ErrResourceRecordNotFound。
	return nil
}

// GetViolation 读取单条违规证据供受控外部动作定位群和用户。
// @param ctx：查询上下文；id：违规记录ID。
// @returns 完整记录、未找到或数据库错误。
// ⚠️副作用说明：执行一次只读查询，不持有事务锁。
func (r *postgresMonitorRepository) GetViolation(ctx context.Context, id int64) (storedViolation, error) {
	stored, err := scanStoredViolation(r.pool.QueryRow(ctx, `SELECT id,message_id,msg_content,group_id,user_id,status,detection_source,risk_score,reason,violations,action_result,message_time,created_at,updated_at,version FROM forbidden_monitor_violation_audits WHERE id=$1`, id))
	// [决策理由] 无行必须映射通用资源404语义。
	if errors.Is(err, pgx.ErrNoRows) {
		return storedViolation{}, management.ErrResourceRecordNotFound
	}

	// >>> 数据演变示例
	// 1. id7存在 -> storedViolation{GroupID,UserID}。
	// 2. id8不存在 -> ErrResourceRecordNotFound。
	return stored, err
}

// TransitionByEvent 将群解禁或踢出事件关联到时间窗内最新违规记录。
// @param ctx：事务上下文；actor：事件审计身份；groupID/userID：关联键；from/to：时间窗；targetStatus：目标状态。
// @returns 是否匹配更新与错误。
// ⚠️副作用说明：锁定记录、更新状态并写管理审计。
func (r *postgresMonitorRepository) TransitionByEvent(ctx context.Context, actor management.Actor, groupID, userID int64, from, to time.Time, targetStatus string) (bool, error) {
	// [决策理由] 群事件仅允许两个最终状态，防止内部调用越权跳转。
	if targetStatus != statusFalsePositive && targetStatus != statusConfirmedKicked {
		return false, management.ErrInvalidResourceData
	}
	tx, err := r.pool.Begin(ctx)
	// [决策理由] 状态和审计必须原子。
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	stored, err := findViolationForEvent(ctx, tx, groupID, userID, from, to)
	// [决策理由] 无匹配是正常事件噪声，应返回false而非404。
	if errors.Is(err, management.ErrResourceRecordNotFound) {
		return false, nil
	}
	// [决策理由] 数据库故障不能被当作无匹配。
	if err != nil {
		return false, err
	}
	after, err := updateViolationStatus(ctx, tx, stored, targetStatus, eventAllowedTransition)
	// [决策理由] 已终结或不合法跳转不得写审计。
	if err != nil {
		return false, err
	}
	// [决策理由] 事件自动状态变化也需可追溯。
	if err := insertViolationAudit(ctx, tx, actor, stored, after); err != nil {
		return false, err
	}
	// [决策理由] 仅全部副作成功才对外确认匹配。
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	// >>> 数据演变示例
	// 1. 解禁+最新pending -> false_positive+审计,true。
	// 2. 时间窗无记录 -> false,nil。
	return true, nil
}

// ListPending 分页读取待人工复核记录。
// @param ctx：查询上下文；query：已校验分页。
// @returns 按创建时间倒序的资源页。
// ⚠️副作用说明：执行两次只读查询。
func (r *postgresMonitorRepository) ListPending(ctx context.Context, query management.ResourceQuery) (management.ResourcePage, error) {
	var total int64
	// [决策理由] 页面需要精确待复核总数。
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM forbidden_monitor_violation_audits WHERE status IN ($1,$2,$3)`, statusPendingReview, statusConfirmedPendingKick, statusFalsePositivePending).Scan(&total); err != nil {
		return management.ResourcePage{}, err
	}
	rows, err := r.pool.Query(ctx, `SELECT id,message_id,msg_content,group_id,user_id,status,detection_source,risk_score,reason,violations,action_result,message_time,created_at,updated_at,version FROM forbidden_monitor_violation_audits WHERE status IN ($1,$2,$3) ORDER BY created_at DESC,id DESC LIMIT $4 OFFSET $5`, statusPendingReview, statusConfirmedPendingKick, statusFalsePositivePending, query.PageSize, (query.Page-1)*query.PageSize)
	// [决策理由] 列表失败不得返回仅含total的误导页。
	if err != nil {
		return management.ResourcePage{}, err
	}
	defer rows.Close()
	items := make([]management.ResourceRecord, 0)
	for rows.Next() {
		record, err := scanViolation(rows)
		// [决策理由] 任一证据行不完整时不得展示部分页。
		if err != nil {
			return management.ResourcePage{}, err
		}
		items = append(items, record)
	}
	// [决策理由] 迭代期间的连接错误必须上报。
	if err := rows.Err(); err != nil {
		return management.ResourcePage{}, err
	}
	result := management.ResourcePage{Items: items, Page: query.Page, PageSize: query.PageSize, Total: total}
	// >>> 数据演变示例
	// 1. pending3+size2 -> items2,total3。
	// 2. 无pending -> 空items,total0。
	return result, nil
}

// Review 使用乐观锁执行WebUI复核状态迁移。
// @param ctx：事务上下文；actor：管理员；id/version：记录与期望版本；targetStatus：复核结果。
// @returns 更新后资源记录或冲突/校验错误。
// ⚠️副作用说明：更新审计状态并写管理审计。
func (r *postgresMonitorRepository) Review(ctx context.Context, actor management.Actor, id, version int64, targetStatus string) (management.ResourceRecord, error) {
	tx, err := r.pool.Begin(ctx)
	// [决策理由] 前像、状态与审计必须原子。
	if err != nil {
		return management.ResourceRecord{}, err
	}
	defer tx.Rollback(ctx)
	stored, err := getViolationForUpdate(ctx, tx, id)
	// [决策理由] 查找错误需区分404与数据库故障。
	if err != nil {
		return management.ResourceRecord{}, err
	}
	// [决策理由] 陈旧页面不得覆盖群事件或其他管理员结果。
	if stored.Version != version {
		return management.ResourceRecord{}, management.ErrResourceConflict
	}
	after, err := updateViolationStatus(ctx, tx, stored, targetStatus, reviewAllowedTransition)
	// [决策理由] 非pending或非允许目标必须被拒绝。
	if err != nil {
		return management.ResourceRecord{}, err
	}
	// [决策理由] 复核操作必须保存不可篡改的前后像。
	if err := insertViolationAudit(ctx, tx, actor, stored, after); err != nil {
		return management.ResourceRecord{}, err
	}
	// [决策理由] 提交是对外返回成功的唯一终点。
	if err := tx.Commit(ctx); err != nil {
		return management.ResourceRecord{}, err
	}
	result, err := after.resourceRecord()
	// >>> 数据演变示例
	// 1. pending v1+确认 -> confirmed_pending_kick v2+审计。
	// 2. pending v1+误报 -> false_positive v2+审计。
	return result, err
}

// BeginFalsePositive 以CAS将待复核记录预占为解禁处理中。
// @param ctx：事务上下文；id/version：记录与页面期望版本。
// @returns 包含群和用户定位信息的处理中记录或冲突错误。
// ⚠️副作用说明：把状态改为false_positive_unban_pending并递增版本，阻止并发复核重复解禁。
func (r *postgresMonitorRepository) BeginFalsePositive(ctx context.Context, id, version int64) (storedViolation, error) {
	tx, err := r.pool.Begin(ctx)
	// [决策理由] 锁定读取和状态预占必须在一个事务中完成。
	if err != nil {
		return storedViolation{}, err
	}
	defer tx.Rollback(ctx)
	stored, err := getViolationForUpdate(ctx, tx, id)
	// [决策理由] 不存在或数据库错误必须在外部解禁前返回。
	if err != nil {
		return storedViolation{}, err
	}
	// [决策理由] 终态提交失败后WebUI会重新展示处理中记录，同版本重试解禁是幂等且可恢复的。
	if stored.Version == version && stored.Data.Status == statusFalsePositivePending {
		return stored, nil
	}
	// [决策理由] 页面版本或状态陈旧时不得对用户执行任何外部动作。
	if stored.Version != version || (stored.Data.Status != statusPendingReview && stored.Data.Status != statusConfirmedPendingKick) {
		return storedViolation{}, management.ErrResourceConflict
	}
	// [决策理由] 同群同用户的解禁是单一外部状态，事务级咨询锁用于串行不同违规记录的预占。
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, fmt.Sprintf("forbidden_unban:%d:%d", stored.Data.GroupID, stored.Data.UserID)); err != nil {
		return storedViolation{}, err
	}
	var activeID int64
	err = tx.QueryRow(ctx, `SELECT id FROM forbidden_monitor_violation_audits WHERE group_id=$1 AND user_id=$2 AND status=$3 AND id<>$4 LIMIT 1`, stored.Data.GroupID, stored.Data.UserID, statusFalsePositivePending, stored.ID).Scan(&activeID)
	// [决策理由] 已有另一条解禁处理中时拒绝当前预占，确保GROUP_BAN只有一个关联目标。
	if err == nil {
		return storedViolation{}, management.ErrResourceConflict
	}
	// [决策理由] 除无行外的查询错误必须阻止外部动作。
	if !errors.Is(err, pgx.ErrNoRows) {
		return storedViolation{}, err
	}
	after, err := updateViolationStatus(ctx, tx, stored, statusFalsePositivePending, func(from, to string) bool {
		return (from == statusPendingReview || from == statusConfirmedPendingKick) && to == statusFalsePositivePending
	})
	// [决策理由] 预占失败时外部动作必须保持未执行。
	if err != nil {
		return storedViolation{}, err
	}
	// [决策理由] 只有已提交的预占才可作为解禁授权。
	if err := tx.Commit(ctx); err != nil {
		return storedViolation{}, err
	}
	// >>> 数据演变示例
	// 1. pending v1 -> pending_unban v2 -> 返回群用户定位。
	// 2. stale v1而数据库v2 -> conflict且不改状态。
	return after, nil
}

// FinishFalsePositive 完成已解禁记录的误报终态与管理审计。
// @param ctx：事务上下文；actor：管理员；id/version：处理中记录及版本。
// @returns 误判终态资源；若解禁事件已抢先完成则返回当前终态资源。
// ⚠️副作用说明：更新状态并写管理审计；全部在一个事务提交。
func (r *postgresMonitorRepository) FinishFalsePositive(ctx context.Context, actor management.Actor, id, version int64) (management.ResourceRecord, error) {
	tx, err := r.pool.Begin(ctx)
	// [决策理由] 终态和审计必须原子提交。
	if err != nil {
		return management.ResourceRecord{}, err
	}
	defer tx.Rollback(ctx)
	stored, err := getViolationForUpdate(ctx, tx, id)
	// [决策理由] 记录读取失败不能生成无前像审计。
	if err != nil {
		return management.ResourceRecord{}, err
	}
	// [决策理由] 机器人解禁通知可能先完成同一状态，此时视为幂等成功。
	if stored.Data.Status == statusFalsePositive {
		return stored.resourceRecord()
	}
	// [决策理由] 只有本次预占版本可完成，防止其他状态变化被覆盖。
	if stored.Version != version || stored.Data.Status != statusFalsePositivePending {
		return management.ResourceRecord{}, management.ErrResourceConflict
	}
	after, err := updateViolationStatus(ctx, tx, stored, statusFalsePositive, func(from, to string) bool {
		return from == statusFalsePositivePending && to == statusFalsePositive
	})
	// [决策理由] 状态更新失败时不得写管理审计。
	if err != nil {
		return management.ResourceRecord{}, err
	}
	// [决策理由] 管理员操作需保存不可变前后像。
	if err := insertViolationAudit(ctx, tx, actor, stored, after); err != nil {
		return management.ResourceRecord{}, err
	}
	// [决策理由] 提交成功是对外确认终态的唯一依据。
	if err := tx.Commit(ctx); err != nil {
		return management.ResourceRecord{}, err
	}
	// >>> 数据演变示例
	// 1. pending_unban v2+解禁成功 -> false_positive v3+审计。
	// 2. 解禁事件已写false_positive -> 直接返回当前记录。
	return after.resourceRecord()
}

// CancelFalsePositive 在外部解禁失败时撤销仍由本次持有的预占。
// @param ctx：查询上下文；id/version：处理中记录及版本。
// @returns 数据库错误；状态已被事件推进时不覆盖并返回nil。
// ⚠️副作用说明：匹配处理中版本时恢复pending_review并递增版本。
func (r *postgresMonitorRepository) CancelFalsePositive(ctx context.Context, id, version int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE forbidden_monitor_violation_audits SET status=$1,version=version+1,updated_at=NOW() WHERE id=$2 AND version=$3 AND status=$4`, statusPendingReview, id, version, statusFalsePositivePending)
	// >>> 数据演变示例
	// 1. pending_unban v2+Action失败 -> pending v3。
	// 2. 已由解禁事件完成 -> 0行更新 -> 保留终态。
	return err
}

// List 将待复核分页委派给仓储。
// @param ctx：查询上下文；actor：已授权管理员；query：分页。
// @returns 待复核资源页。
// ⚠️副作用说明：查询数据库。
func (h *violationResourceHandler) List(ctx context.Context, _ management.Actor, query management.ResourceQuery) (management.ResourcePage, error) {
	result, err := h.repository.ListPending(ctx, query)
	// >>> 数据演变示例
	// 1. page1 -> pending页。
	// 2. DB错误 -> 空页,error。
	return result, err
}

// Create 拒绝管理端伪造违规证据。
// @param ctx/actor/raw：管理请求参数，均不用于创建。
// @returns ErrInvalidResourceData。
// ⚠️副作用说明：无。
func (h *violationResourceHandler) Create(context.Context, management.Actor, json.RawMessage) (management.ResourceRecord, error) {
	// >>> 数据演变示例
	// 1. WebUI POST -> 拒绝。
	// 2. 空JSON -> 拒绝。
	return management.ResourceRecord{}, management.ErrInvalidResourceData
}

// Update 仅接受待复核记录的确认或误报状态。
// @param ctx：请求上下文；actor：管理员；id/version：记录与版本；raw：仅含status的JSON。
// @returns 复核后记录或校验/冲突错误。
// ⚠️副作用说明：成功时委派仓储执行原子复核。
func (h *violationResourceHandler) Update(ctx context.Context, actor management.Actor, id, version int64, raw json.RawMessage) (management.ResourceRecord, error) {
	status, err := decodeReviewStatus(raw)
	// [决策理由] 输入与标识必须在开启事务前完成校验。
	if err != nil || id < 1 || version < 1 {
		if err != nil {
			return management.ResourceRecord{}, err
		}
		return management.ResourceRecord{}, management.ErrInvalidResourceData
	}
	// [决策理由] “误报-已解禁”终态必须以OneBot解除禁言成功为前提。
	if status == statusFalsePositive {
		current, getErr := h.repository.GetViolation(ctx, id)
		// [决策理由] 必须先读取处置结果和版本，才能判断是否真的需要调用群解禁。
		if getErr != nil {
			return management.ResourceRecord{}, getErr
		}
		// [决策理由] 陈旧页面或非待复核状态不得绕过后续CAS状态机。
		if current.Version != version || (current.Data.Status != statusPendingReview && current.Data.Status != statusConfirmedPendingKick) {
			return management.ResourceRecord{}, management.ErrResourceConflict
		}
		var outcome moderationOutcome
		// [决策理由] 处置摘要损坏时不能冒险调用外部解禁或声称无禁言。
		if err := json.Unmarshal(current.Data.ActionResult, &outcome); err != nil {
			return management.ResourceRecord{}, fmt.Errorf("解析违规处置结果: %w", err)
		}
		// [决策理由] 未自动禁言的待复核记录只需原子沉淀反馈，不能发送多余解禁Action。
		if !outcome.BanSucceeded {
			return h.repository.Review(ctx, actor, id, version, statusFalsePositive)
		}
		stored, beginErr := h.repository.BeginFalsePositive(ctx, id, version)
		// [决策理由] 记录必须先完成CAS预占，陈旧请求绝不能触发解禁。
		if beginErr != nil {
			return management.ResourceRecord{}, beginErr
		}
		// [决策理由] 缺少Action依赖时不得写入名不副实的已解禁状态。
		if h.actions == nil {
			_ = h.repository.CancelFalsePositive(ctx, id, stored.Version)
			return management.ResourceRecord{}, fmt.Errorf("违规复核缺少 ActionAPI")
		}
		actionContext, cancel := context.WithTimeout(ctx, actionTimeout)
		unbanErr := h.actions.SetGroupBan(actionContext, onebot.SetGroupBanParams{GroupID: strconv.FormatInt(stored.Data.GroupID, 10), UserID: strconv.FormatInt(stored.Data.UserID, 10), Duration: 0})
		cancel()
		// [决策理由] 解禁失败时保留pending状态供管理员重试。
		if unbanErr != nil {
			cancelErr := h.repository.CancelFalsePositive(ctx, id, stored.Version)
			// [决策理由] 补偿失败意味着记录仍处于处理中，需连同原始Action失败一起暴露。
			if cancelErr != nil {
				return management.ResourceRecord{}, errors.Join(fmt.Errorf("解除误报用户禁言: %w", unbanErr), fmt.Errorf("撤销误报预占: %w", cancelErr))
			}
			return management.ResourceRecord{}, fmt.Errorf("解除误报用户禁言: %w", unbanErr)
		}
		return h.repository.FinishFalsePositive(ctx, actor, id, stored.Version)
	}
	result, err := h.repository.Review(ctx, actor, id, version, status)
	// >>> 数据演变示例
	// 1. {status:confirmed_pending_kick}+v1 -> Review -> v2。
	// 2. {msg_content:x} -> 未知字段 -> 不访问仓储。
	return result, err
}

// Delete 拒绝删除违规审计证据。
// @param ctx/actor/id/version：管理请求参数。
// @returns ErrInvalidResourceData。
// ⚠️副作用说明：无。
func (h *violationResourceHandler) Delete(context.Context, management.Actor, int64, int64) error {
	// >>> 数据演变示例
	// 1. DELETE id1 -> 拒绝。
	// 2. DELETE id0 -> 拒绝。
	return management.ErrInvalidResourceData
}

// decodeReviewStatus 严格解码受限复核输入。
// @param raw：仅允许status字段的JSON。
// @returns 合法目标状态或ErrInvalidResourceData。
// ⚠️副作用说明：无。
func decodeReviewStatus(raw json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload reviewPayload
	// [决策理由] 非对象、类型错误和未知字段不得进入复核事务。
	if err := decoder.Decode(&payload); err != nil {
		return "", fmt.Errorf("%w: %v", management.ErrInvalidResourceData, err)
	}
	var extra any
	err := decoder.Decode(&extra)
	// [决策理由] 只有EOF表示载荷中没有第二个JSON值。
	if !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("%w: 必须仅提交一个JSON对象", management.ErrInvalidResourceData)
	}
	// [决策理由] WebUI只呈现中文动作，后端映射为稳定数据库状态。
	if payload.Status == "确认" {
		payload.Status = statusConfirmedPendingKick
	} else if payload.Status == "误报" {
		payload.Status = statusFalsePositive
	} else {
		return "", fmt.Errorf("%w: 不支持的复核状态", management.ErrInvalidResourceData)
	}
	// >>> 数据演变示例
	// 1. {status:false_positive_unbanned} -> 合法状态。
	// 2. {status:confirmed_kicked} -> 拒绝WebUI越权跳转。
	return payload.Status, nil
}

type transitionRule func(string, string) bool

// reviewAllowedTransition 限制WebUI复核状态机。
// @param from/to：当前与目标状态。
// @returns 是否允许迁移。
// ⚠️副作用说明：无。
func reviewAllowedTransition(from, to string) bool {
	result := (from == statusPendingReview && (to == statusConfirmedPendingKick || to == statusFalsePositive)) || (from == statusConfirmedPendingKick && to == statusFalsePositive)
	// >>> 数据演变示例
	// 1. pending->confirmed_pending_kick -> true。
	// 2. pending->false_positive -> true；confirmed_kicked->false_positive -> false。
	return result
}

// eventAllowedTransition 限制群管理事件状态机。
// @param from/to：当前与目标状态。
// @returns 是否允许迁移。
// ⚠️副作用说明：无。
func eventAllowedTransition(from, to string) bool {
	result := (to == statusFalsePositive && (from == statusPendingReview || from == statusConfirmedPendingKick || from == statusFalsePositivePending)) || (to == statusConfirmedKicked && (from == statusPendingReview || from == statusConfirmedPendingKick))
	// >>> 数据演变示例
	// 1. confirmed_pending_kick->confirmed_kicked -> true。
	// 2. false_positive->confirmed_kicked -> false。
	return result
}

// updateViolationStatus 执行已锁定记录的CAS状态迁移。
// @param ctx/tx：事务；before：锁定前像；target：目标状态；allowed：状态机。
// @returns 递增版本后像或冲突。
// ⚠️副作用说明：更新违规审计行。
func updateViolationStatus(ctx context.Context, tx pgx.Tx, before storedViolation, target string, allowed transitionRule) (storedViolation, error) {
	// [决策理由] 状态机是数据库写入前的领域边界。
	if !allowed(before.Data.Status, target) {
		return storedViolation{}, management.ErrResourceConflict
	}
	after := before
	err := tx.QueryRow(ctx, `UPDATE forbidden_monitor_violation_audits SET status=$1,version=version+1,updated_at=NOW() WHERE id=$2 AND version=$3 RETURNING version,updated_at`, target, before.ID, before.Version).Scan(&after.Version, &after.Data.UpdatedAt)
	// [决策理由] CAS无行表示版本已变化。
	if errors.Is(err, pgx.ErrNoRows) {
		return storedViolation{}, management.ErrResourceConflict
	}
	// [决策理由] 其他数据库错误必须保留。
	if err != nil {
		return storedViolation{}, err
	}
	after.Data.Status = target
	// >>> 数据演变示例
	// 1. pending v1->confirmed -> v2后像。
	// 2. 已终结->任意目标 -> conflict。
	return after, nil
}

// getViolationForUpdate 按ID锁定违规记录。
// @param ctx/tx：事务；id：记录ID。
// @returns 完整前像、未找到或数据库错误。
// ⚠️副作用说明：持有行锁至事务结束。
func getViolationForUpdate(ctx context.Context, tx pgx.Tx, id int64) (storedViolation, error) {
	stored, err := scanStoredViolation(tx.QueryRow(ctx, `SELECT id,message_id,msg_content,group_id,user_id,status,detection_source,risk_score,reason,violations,action_result,message_time,created_at,updated_at,version FROM forbidden_monitor_violation_audits WHERE id=$1 FOR UPDATE`, id))
	// [决策理由] 无行需映射稳定404语义。
	if errors.Is(err, pgx.ErrNoRows) {
		return storedViolation{}, management.ErrResourceRecordNotFound
	}
	// >>> 数据演变示例
	// 1. id7 -> 锁定完整记录。
	// 2. id8不存在 -> ErrResourceRecordNotFound。
	return stored, err
}

// findViolationForEvent 锁定时间窗内最新的未终结记录。
// @param ctx/tx：事务；groupID/userID：关联键；from/to：包含边界时间窗。
// @returns 锁定记录、未找到或数据库错误。
// ⚠️副作用说明：持有行锁至事务结束。
func findViolationForEvent(ctx context.Context, tx pgx.Tx, groupID, userID int64, from, to time.Time) (storedViolation, error) {
	stored, err := scanStoredViolation(tx.QueryRow(ctx, `SELECT id,message_id,msg_content,group_id,user_id,status,detection_source,risk_score,reason,violations,action_result,message_time,created_at,updated_at,version FROM forbidden_monitor_violation_audits WHERE group_id=$1 AND user_id=$2 AND message_time BETWEEN $3 AND $4 AND status IN ($5,$6,$7) ORDER BY (status=$7) DESC,message_time DESC,id DESC LIMIT 1 FOR UPDATE`, groupID, userID, from, to, statusPendingReview, statusConfirmedPendingKick, statusFalsePositivePending))
	// [决策理由] 无匹配转为领域未找到供上层当作噪声。
	if errors.Is(err, pgx.ErrNoRows) {
		return storedViolation{}, management.ErrResourceRecordNotFound
	}
	// >>> 数据演变示例
	// 1. 窗内两条pending -> 锁定最新一条。
	// 2. 仅有已踢出 -> ErrResourceRecordNotFound。
	return stored, err
}

// scanStoredViolation 扫描固定违规记录列。
// @param row：提供15列的扫描源。
// @returns 完整存储记录或扫描错误。
// ⚠️副作用说明：读取当前行。
func scanStoredViolation(row pgx.Row) (storedViolation, error) {
	var stored storedViolation
	err := row.Scan(&stored.ID, &stored.Data.MessageID, &stored.Data.MessageContent, &stored.Data.GroupID, &stored.Data.UserID, &stored.Data.Status, &stored.Data.DetectionSource, &stored.Data.RiskScore, &stored.Data.Reason, &stored.Data.Violations, &stored.Data.ActionResult, &stored.Data.MessageTime, &stored.Data.CreatedAt, &stored.Data.UpdatedAt, &stored.Version)
	// >>> 数据演变示例
	// 1. 15列合法 -> storedViolation。
	// 2. JSON列类型错误 -> Scan error。
	return stored, err
}

// scanViolation 将查询行转换为通用资源记录。
// @param row：固定列扫描源。
// @returns JSON资源记录或错误。
// ⚠️副作用说明：读行并分配JSON。
func scanViolation(row pgx.Row) (management.ResourceRecord, error) {
	stored, err := scanStoredViolation(row)
	// [决策理由] 扫描失败时不得序列化零值证据。
	if err != nil {
		return management.ResourceRecord{}, err
	}
	result, err := stored.resourceRecord()
	// >>> 数据演变示例
	// 1. stored id7,v1 -> ResourceRecord。
	// 2. 扫描失败 -> 空记录,error。
	return result, err
}

// resourceRecord 序列化违规证据资源。
// @param 无；接收者包含记录前像或后像。
// @returns 通用资源记录。
// ⚠️副作用说明：分配JSON字节。
func (s storedViolation) resourceRecord() (management.ResourceRecord, error) {
	data := map[string]any{
		"message_id":       s.Data.MessageID,
		"msg_content":      s.Data.MessageContent,
		"group_id":         strconv.FormatInt(s.Data.GroupID, 10),
		"user_id":          strconv.FormatInt(s.Data.UserID, 10),
		"status":           statusDisplayName(s.Data.Status),
		"detection_source": s.Data.DetectionSource,
		"risk_score":       s.Data.RiskScore,
		"reason":           s.Data.Reason,
		"violations":       s.Data.Violations,
		"action_result":    s.Data.ActionResult,
		"message_time":     s.Data.MessageTime,
		"created_at":       s.Data.CreatedAt,
		"updated_at":       s.Data.UpdatedAt,
	}
	raw, err := json.Marshal(data)
	// [决策理由] 序列化失败不得返回缺失证据的记录。
	if err != nil {
		return management.ResourceRecord{}, err
	}
	result := management.ResourceRecord{ID: s.ID, Version: s.Version, Data: raw}
	// >>> 数据演变示例
	// 1. id7,v2,pending -> record{7,2,JSON}。
	// 2. 零值 -> record{0,0,JSON}。
	return result, nil
}

// statusDisplayName 将稳定状态转换为WebUI中文文本。
// @param status：数据库状态值。
// @returns 对应中文状态，未知值原样返回便于诊断。
// ⚠️副作用说明：无。
func statusDisplayName(status string) string {
	result := status
	switch status {
	case statusPendingReview:
		result = "待人工复核"
	case statusConfirmedPendingKick:
		result = "已确认-待踢出"
	case statusConfirmedKicked:
		result = "已确认-已踢出"
	case statusFalsePositivePending:
		result = "误判-解禁处理中"
	case statusFalsePositive:
		result = "误判-已解禁"
	}

	// >>> 数据演变示例
	// 1. pending_review -> 待人工复核。
	// 2. future_status -> future_status。
	return result
}

// insertViolationAudit 写入违规复核的管理审计前后像。
// @param ctx/tx：事务；actor：操作身份；before/after：状态前后像。
// @returns 序列化或SQL错误。
// ⚠️副作用说明：插入admin_audit_logs，随调用方事务提交。
func insertViolationAudit(ctx context.Context, tx pgx.Tx, actor management.Actor, before, after storedViolation) error {
	beforeRecord, err := before.resourceRecord()
	// [决策理由] 不可生成完整前像时不得写成功审计。
	if err != nil {
		return err
	}
	afterRecord, err := after.resourceRecord()
	// [决策理由] 不可生成完整后像时不得提交业务更新。
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO admin_audit_logs(actor_id,actor_role,channel,action,target_type,target_id,before_json,after_json,success,request_id) VALUES($1,$2,$3,'plugin.resource.update','forbidden_monitor_violation',$4,$5::jsonb,$6::jsonb,TRUE,NULLIF($7,''))`, actor.ID, actor.Role, actor.Channel, fmt.Sprintf("%d", before.ID), beforeRecord.Data, afterRecord.Data, actor.RequestID)
	// >>> 数据演变示例
	// 1. pending->confirmed -> 审计含前后证据。
	// 2. SQL失败 -> 返回错误促使业务回滚。
	return err
}
