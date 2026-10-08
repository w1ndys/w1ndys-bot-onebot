// 📌 影响范围：读写 PostgreSQL 违禁监控词库业务表并在同一事务写入审计；不访问外部模型。
package forbiddenmessagemonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/w1ndys-bot/internal/management"
	"github.com/w1ndys/w1ndys-bot/internal/plugin"
)

var (
	// ErrLexiconNotFound 表示词条不存在。
	ErrLexiconNotFound = errors.New("违禁词库记录不存在")
	// ErrLexiconConflict 表示词条重复或乐观锁版本陈旧。
	ErrLexiconConflict = errors.New("违禁词库记录冲突")
	// ErrInvalidLexicon 表示词库输入未通过领域校验。
	ErrInvalidLexicon = errors.New("违禁词库输入无效")
)

const (
	maxTermRunes       = 100
	maxTermWeight      = 1000
	maxLexiconPageSize = 200
)

// Term 是一条词库词条的管理视图。
type Term struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	Weight    float64   `json:"weight"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TermInput 是新增或更新词条的领域输入。
type TermInput struct {
	Text   string  `json:"text"`
	Weight float64 `json:"weight"`
}

// Lexicon 是发布到检测引擎的完整词库快照。
type Lexicon struct {
	Terms []WeightedKeyword
}

// lexiconLoader 是运行时重建检测引擎所需的最小词库读取能力。
type lexiconLoader interface {
	Load(context.Context) (Lexicon, error)
}

type lexiconDatabase interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type lexiconRepository struct {
	database lexiconDatabase
}

func newLexiconRepository(pool *pgxpool.Pool) (*lexiconRepository, error) {
	if pool == nil {
		return nil, errors.New("违禁词库数据库连接池不能为空")
	}
	return &lexiconRepository{database: pool}, nil
}

// Load 读取完整词库，供生命周期与写入后重建检测引擎。
func (r *lexiconRepository) Load(ctx context.Context) (Lexicon, error) {
	lexicon := Lexicon{Terms: []WeightedKeyword{}}
	rows, err := r.database.Query(ctx, `SELECT text,weight FROM forbidden_monitor_terms ORDER BY id`)
	if err != nil {
		return Lexicon{}, fmt.Errorf("查询违禁词条: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		var weight float64
		if err := rows.Scan(&text, &weight); err != nil {
			return Lexicon{}, fmt.Errorf("扫描违禁词条: %w", err)
		}
		lexicon.Terms = append(lexicon.Terms, WeightedKeyword{Text: text, Weight: weight})
	}
	if err := rows.Err(); err != nil {
		return Lexicon{}, fmt.Errorf("遍历违禁词条: %w", err)
	}
	return lexicon, nil
}

// ListTerms 分页读取加权违禁词。
func (r *lexiconRepository) ListTerms(ctx context.Context, page, pageSize int) ([]Term, int64, error) {
	var total int64
	if err := r.database.QueryRow(ctx, `SELECT COUNT(*) FROM forbidden_monitor_terms`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("统计违禁词条: %w", err)
	}
	rows, err := r.database.Query(ctx, `SELECT id,text,weight,version,updated_at FROM forbidden_monitor_terms
ORDER BY id LIMIT $1 OFFSET $2`, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("查询违禁词条: %w", err)
	}
	defer rows.Close()
	items := make([]Term, 0)
	for rows.Next() {
		var term Term
		if err := rows.Scan(&term.ID, &term.Text, &term.Weight, &term.Version, &term.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("扫描违禁词条: %w", err)
		}
		term.UpdatedAt = term.UpdatedAt.UTC()
		items = append(items, term)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("遍历违禁词条: %w", err)
	}
	return items, total, nil
}

// CreateTerm 新增词条并写入审计。
func (r *lexiconRepository) CreateTerm(ctx context.Context, actor management.Actor, input TermInput) (Term, error) {
	tx, err := r.database.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Term{}, fmt.Errorf("开启违禁词条事务: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var term Term
	err = tx.QueryRow(ctx, `INSERT INTO forbidden_monitor_terms(text,weight)
VALUES($1,$2) RETURNING id,text,weight,version,updated_at`, input.Text, input.Weight).
		Scan(&term.ID, &term.Text, &term.Weight, &term.Version, &term.UpdatedAt)
	if err != nil {
		return Term{}, mapLexiconError(err)
	}
	if err := recordLexiconAudit(ctx, tx, actor, "plugin.forbidden_monitor.term.create", "forbidden_monitor_term", term.ID, nil, term); err != nil {
		return Term{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Term{}, fmt.Errorf("提交违禁词条事务: %w", err)
	}
	term.UpdatedAt = term.UpdatedAt.UTC()
	return term, nil
}

// UpdateTerm 按乐观锁更新词条并写入审计。
func (r *lexiconRepository) UpdateTerm(ctx context.Context, actor management.Actor, id, expectedVersion int64, input TermInput) (Term, error) {
	tx, err := r.database.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Term{}, fmt.Errorf("开启违禁词条事务: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var before Term
	err = tx.QueryRow(ctx, `SELECT id,text,weight,version,updated_at FROM forbidden_monitor_terms WHERE id=$1 FOR UPDATE`, id).
		Scan(&before.ID, &before.Text, &before.Weight, &before.Version, &before.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Term{}, fmt.Errorf("%w: 词条 %d", ErrLexiconNotFound, id)
	}
	if err != nil {
		return Term{}, fmt.Errorf("锁定违禁词条: %w", err)
	}
	if before.Version != expectedVersion {
		return Term{}, ErrLexiconConflict
	}
	var term Term
	err = tx.QueryRow(ctx, `UPDATE forbidden_monitor_terms
SET text=$3,weight=$4,version=version+1,updated_at=NOW()
WHERE id=$1 AND version=$2 RETURNING id,text,weight,version,updated_at`, id, expectedVersion, input.Text, input.Weight).
		Scan(&term.ID, &term.Text, &term.Weight, &term.Version, &term.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Term{}, ErrLexiconConflict
	}
	if err != nil {
		return Term{}, mapLexiconError(err)
	}
	if err := recordLexiconAudit(ctx, tx, actor, "plugin.forbidden_monitor.term.update", "forbidden_monitor_term", id, before, term); err != nil {
		return Term{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Term{}, fmt.Errorf("提交违禁词条事务: %w", err)
	}
	term.UpdatedAt = term.UpdatedAt.UTC()
	return term, nil
}

// DeleteTerm 按乐观锁删除词条并写入审计。
func (r *lexiconRepository) DeleteTerm(ctx context.Context, actor management.Actor, id, expectedVersion int64) error {
	tx, err := r.database.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("开启违禁词条删除事务: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var before Term
	err = tx.QueryRow(ctx, `SELECT id,text,weight,version,updated_at FROM forbidden_monitor_terms WHERE id=$1 FOR UPDATE`, id).
		Scan(&before.ID, &before.Text, &before.Weight, &before.Version, &before.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: 词条 %d", ErrLexiconNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("锁定违禁词条: %w", err)
	}
	if before.Version != expectedVersion {
		return ErrLexiconConflict
	}
	tag, err := tx.Exec(ctx, `DELETE FROM forbidden_monitor_terms WHERE id=$1 AND version=$2`, id, expectedVersion)
	if err != nil {
		return fmt.Errorf("删除违禁词条: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrLexiconConflict
	}
	if err := recordLexiconAudit(ctx, tx, actor, "plugin.forbidden_monitor.term.delete", "forbidden_monitor_term", id, before, nil); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交违禁词条删除事务: %w", err)
	}
	return nil
}

func mapLexiconError(err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23505":
			return ErrLexiconConflict
		case "23514":
			return fmt.Errorf("%w: 违反数据库约束 %s", ErrInvalidLexicon, postgresError.ConstraintName)
		}
	}
	return fmt.Errorf("保存违禁词库记录: %w", err)
}

// recordLexiconAudit 在词库写入所在事务追加审计；before 或 after 为 nil 表示新增或删除。
func recordLexiconAudit(ctx context.Context, tx pgx.Tx, actor management.Actor, action, targetType string, id int64, before, after any) error {
	encode := func(value any) ([]byte, error) {
		if value == nil {
			return nil, nil
		}
		return json.Marshal(value)
	}
	beforeJSON, err := encode(before)
	if err != nil {
		return fmt.Errorf("序列化 %s 审计旧值: %w", action, err)
	}
	afterJSON, err := encode(after)
	if err != nil {
		return fmt.Errorf("序列化 %s 审计新值: %w", action, err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO admin_audit_logs(actor_id,actor_role,channel,action,target_type,target_id,before_json,after_json,success,request_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,TRUE,NULLIF($9,''))`,
		actor.ID, actor.Role, actor.Channel, action, targetType, fmt.Sprintf("%d", id), beforeJSON, afterJSON, actor.RequestID)
	// 审计失败必须回滚词库变更，避免检测规则被无法追溯地修改。
	if err != nil {
		return fmt.Errorf("写入 %s 审计: %w", action, err)
	}
	return nil
}

// currentConfigJSON 返回最近一次成功应用的配置；尚未应用时回退到 Schema 默认值。
func (p *implementation) currentConfigJSON() json.RawMessage {
	current := p.configJSON.Load()
	if current != nil {
		return *current
	}
	// 平台会在插件进入 Ready 前应用配置；此处回退保证直接构造的实例也能得到可运行阈值。
	defaults, err := plugin.NormalizeConfig(p.ConfigSchema(), json.RawMessage(`{}`))
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return defaults
}

// currentLexicon 返回当前已发布的词库快照；尚未加载时返回空词库。
func (p *implementation) currentLexicon() Lexicon {
	current := p.lexicon.Load()
	if current == nil {
		return Lexicon{Terms: []WeightedKeyword{}}
	}
	return *current
}

// reloadLexicon 从业务表重建词库并按当前配置重新发布检测引擎。
// 读取或构造失败时保留旧词库与旧引擎，避免部分词库静默失效。
func (p *implementation) reloadLexicon(ctx context.Context) error {
	p.publicationMu.Lock()
	defer p.publicationMu.Unlock()
	raw := p.currentConfigJSON()
	if p.lexiconStore == nil {
		return errors.New("违禁词库仓库未初始化")
	}
	loaded, err := p.lexiconStore.Load(ctx)
	if err != nil {
		return err
	}
	next, err := buildRuntimeSnapshot(raw, loaded, p.httpClient)
	if err != nil {
		return fmt.Errorf("按新词库重建检测引擎: %w", err)
	}
	p.lexicon.Store(&loaded)
	p.snapshot.Store(next)
	return nil
}
