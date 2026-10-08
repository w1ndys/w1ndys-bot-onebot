// 📌 影响范围：定义目标插件架构的编译期规格、代码角色、群命令和群观察器契约；不访问数据库或外部变量。
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	commandregistry "github.com/w1ndys/w1ndys-bot/internal/command"
	"github.com/w1ndys/w1ndys-bot/internal/ws"
)

var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Role 是目标插件架构允许在代码中声明的封闭群身份。
type Role string

const (
	RoleSuperAdmin  Role = "super_admin"
	RoleGroupOwner  Role = "group_owner"
	RoleGroupAdmin  Role = "group_admin"
	RoleGroupMember Role = "group_member"
)

// RoleSet 表示命令显式允许的身份集合。
type RoleSet map[Role]struct{}

// Roles 构造不共享调用方状态的身份集合。
// @param roles：命令允许的封闭身份列表。
// @returns 去重后的身份集合。
// ⚠️副作用说明：分配新的映射；不修改输入。
func Roles(roles ...Role) RoleSet {
	result := make(RoleSet, len(roles))
	for _, role := range roles {
		result[role] = struct{}{}
	}

	// >>> 数据演变示例
	// 1. [group_member,group_admin] -> map{group_member,group_admin}。
	// 2. [group_member,group_member] -> 去重 -> map{group_member}。
	return result
}

// Contains 判断身份是否被命令显式允许。
// @param role：待判断的当前群身份。
// @returns 身份存在于集合时为 true。
// ⚠️副作用说明：无。
func (r RoleSet) Contains(role Role) bool {
	_, found := r[role]

	// >>> 数据演变示例
	// 1. {group_member}+group_member -> true。
	// 2. {group_admin}+group_member -> false。
	return found
}

// CommandScope 是命令支持的消息作用域。
type CommandScope string

const CommandScopeGroup CommandScope = "group"

// CommandContext 是 Dispatcher 完成匹配、门禁和身份授权后的命令输入。
type CommandContext struct {
	Context   context.Context
	Message   *ws.MessageEvent
	Trigger   string
	Arguments string
	Role      Role
}

// CommandHandler 处理一个已经授权的群命令。
type CommandHandler func(CommandContext) error

// CommandSpec 声明稳定命令、触发词、作用域、代码身份和处理器。
type CommandSpec struct {
	Key          string
	DisplayName  string
	Description  string
	Triggers     []string
	Scope        CommandScope
	AllowedRoles RoleSet
	Handler      CommandHandler
}

// ObserverEventKind 是平台允许观察器订阅的群事件类别。
type ObserverEventKind string

const (
	ObserverGroupMessage ObserverEventKind = "group_message"
	ObserverGroupNotice  ObserverEventKind = "group_notice"
	ObserverGroupRequest ObserverEventKind = "group_request"
)

// ObserverPhase 表示观察器相对命令匹配的执行阶段。
type ObserverPhase string

const (
	// ObserverPhaseAfterCommands 保持观察器仅接收未命中命令事件的默认行为。
	ObserverPhaseAfterCommands ObserverPhase = ""
	// ObserverPhaseBeforeCommands 在命令匹配前观察所有通过门禁的群事件。
	ObserverPhaseBeforeCommands ObserverPhase = "before_commands"
)

// ObserverContext 是 Dispatcher 完成全局和群门禁后的观察事件输入。
type ObserverContext struct {
	Context context.Context
	GroupID int64
	Event   ws.Event
}

// ObserverHandler 处理一个已经通过运行门禁的群事件。
type ObserverHandler func(ObserverContext) error

// ObserverSpec 声明观察器稳定标识、执行阶段、群事件类别和处理器。
type ObserverSpec struct {
	Key         string
	Description string
	Phase       ObserverPhase
	EventKinds  []ObserverEventKind
	Handler     ObserverHandler
}

// Lifecycle 是纯后台插件或有资源插件的可选启停契约。
type Lifecycle interface {
	OnEnable(context.Context) error
	OnDisable(context.Context) error
}

// ConfigSpec 声明插件的小型标量配置及其领域校验与热应用钩子。
//
// Validate 可选，必须无副作用且并发安全；Apply 必须原子发布不可变快照，
// 返回错误或 panic 时不得改变旧快照。两者都不得在消息热路径上被调用。
type ConfigSpec struct {
	Schema   ConfigSchema
	Validate func(context.Context, json.RawMessage) error
	Apply    func(context.Context, json.RawMessage) error
}

// PluginSpec 是目标架构中由代码持有的完整插件规格。
type PluginSpec struct {
	Key          string
	DisplayName  string
	Description  string
	Commands     []CommandSpec
	Observers    []ObserverSpec
	Config       *ConfigSpec
	Lifecycle    Lifecycle
	AdminPageKey string
}

// Validate 校验单个插件规格的稳定标识、入口和安全声明。
// @param 无。
// @returns 首个无效标识、命令、观察器、角色或入口错误。
// ⚠️副作用说明：无；仅检查规格并标准化临时触发词。
func (s PluginSpec) Validate() error {
	// [决策理由] 插件 Key 会进入 Catalog、状态表、API 和审计，必须是稳定机器标识。
	if !identifierPattern.MatchString(s.Key) {
		return fmt.Errorf("无效插件 Key %q", s.Key)
	}
	// [决策理由] 管理界面必须能用可读名称区分插件。
	if strings.TrimSpace(s.DisplayName) == "" {
		return errors.New("插件展示名称不能为空")
	}
	// [决策理由] 管理员需要说明判断插件用途和开启影响。
	if strings.TrimSpace(s.Description) == "" {
		return errors.New("插件说明不能为空")
	}
	// [决策理由] 没有消息入口和生命周期的规格不能产生任何行为，通常表示注册遗漏。
	if len(s.Commands) == 0 && len(s.Observers) == 0 && s.Lifecycle == nil {
		return fmt.Errorf("插件 %s 至少需要命令、观察器或生命周期入口", s.Key)
	}
	// [决策理由] 专属页面键由前后端编译期注册表共同引用，非空时必须稳定。
	if s.AdminPageKey != "" && !identifierPattern.MatchString(s.AdminPageKey) {
		return fmt.Errorf("插件 %s 包含无效管理页面 Key %q", s.Key, s.AdminPageKey)
	}
	seenEntries := make(map[string]string, len(s.Commands)+len(s.Observers))
	seenTriggers := make(map[string]string)
	for _, command := range s.Commands {
		// [决策理由] 命令 Key 是代码授权、测试和审计的稳定引用。
		if !identifierPattern.MatchString(command.Key) {
			return fmt.Errorf("插件 %s 包含无效命令 Key %q", s.Key, command.Key)
		}
		// [决策理由] 命令和观察器共享插件入口命名空间，避免管理与日志引用歧义。
		if owner, found := seenEntries[command.Key]; found {
			return fmt.Errorf("插件 %s 的入口 %q 重复（%s 与命令）", s.Key, command.Key, owner)
		}
		seenEntries[command.Key] = "命令"
		// [决策理由] 命令展示名称和说明是只读 WebUI 及审查权限边界的必要信息。
		if strings.TrimSpace(command.DisplayName) == "" || strings.TrimSpace(command.Description) == "" {
			return fmt.Errorf("插件 %s 的命令 %s 展示名称或说明为空", s.Key, command.Key)
		}
		// [决策理由] 目标架构首版只允许群命令，禁止私聊静默绕过群门禁。
		if command.Scope != CommandScopeGroup {
			return fmt.Errorf("插件 %s 的命令 %s 作用域 %q 不受支持", s.Key, command.Key, command.Scope)
		}
		// [决策理由] 没有触发词的命令永远无法由 Dispatcher 匹配。
		if len(command.Triggers) == 0 {
			return fmt.Errorf("插件 %s 的命令 %s 至少需要一个触发词", s.Key, command.Key)
		}
		// [决策理由] 空身份集合必须 fail-closed 且在启动时暴露，而不是形成不可解释命令。
		if len(command.AllowedRoles) == 0 {
			return fmt.Errorf("插件 %s 的命令 %s 必须声明允许身份", s.Key, command.Key)
		}
		for role := range command.AllowedRoles {
			// [决策理由] 只允许封闭身份集合，避免拼写错误产生隐式权限语义。
			if !validRole(role) {
				return fmt.Errorf("插件 %s 的命令 %s 包含未知身份 %q", s.Key, command.Key, role)
			}
		}
		// [决策理由] nil Handler 会在命令命中后 panic，必须在 Catalog 构建前拒绝。
		if command.Handler == nil {
			return fmt.Errorf("插件 %s 的命令 %s Handler 不能为空", s.Key, command.Key)
		}
		for _, trigger := range command.Triggers {
			normalized, err := commandregistry.Normalize(trigger, "")
			// [决策理由] 规格校验与未来 Dispatcher 必须共享相同触发词规则。
			if err != nil {
				return fmt.Errorf("插件 %s 的命令 %s 触发词 %q 无效: %w", s.Key, command.Key, trigger, err)
			}
			owner, found := seenTriggers[normalized]
			// [决策理由] 同一插件内标准化后重复会导致路由目标不确定。
			if found {
				return fmt.Errorf("插件 %s 的触发词 %q 重复（命令 %s 与 %s）", s.Key, normalized, owner, command.Key)
			}
			seenTriggers[normalized] = command.Key
		}
	}
	for _, observer := range s.Observers {
		// [决策理由] 观察器 Key 是事件路由、错误和测试的稳定引用。
		if !identifierPattern.MatchString(observer.Key) {
			return fmt.Errorf("插件 %s 包含无效观察器 Key %q", s.Key, observer.Key)
		}
		// [决策理由] 所有入口使用同一命名空间，防止同名命令和观察器难以定位。
		if owner, found := seenEntries[observer.Key]; found {
			return fmt.Errorf("插件 %s 的入口 %q 重复（%s 与观察器）", s.Key, observer.Key, owner)
		}
		seenEntries[observer.Key] = "观察器"
		// [决策理由] 观察器说明用于审查广播事件的范围和成本。
		if strings.TrimSpace(observer.Description) == "" {
			return fmt.Errorf("插件 %s 的观察器 %s 说明为空", s.Key, observer.Key)
		}
		// [决策理由] 未知阶段可能让安全观察器在命令前后错误执行，必须在启动时拒绝。
		if !validObserverPhase(observer.Phase) {
			return fmt.Errorf("插件 %s 的观察器 %s 包含未知执行阶段 %q", s.Key, observer.Key, observer.Phase)
		}
		// [决策理由] 空事件集合无法形成可审查的观察范围。
		if len(observer.EventKinds) == 0 {
			return fmt.Errorf("插件 %s 的观察器 %s 至少需要一个群事件类型", s.Key, observer.Key)
		}
		// [决策理由] nil Handler 会在事件分发时 panic，必须提前拒绝。
		if observer.Handler == nil {
			return fmt.Errorf("插件 %s 的观察器 %s Handler 不能为空", s.Key, observer.Key)
		}
		seenKinds := make(map[ObserverEventKind]struct{}, len(observer.EventKinds))
		for _, kind := range observer.EventKinds {
			// [决策理由] 未知事件类型会让插件作者误以为观察器已订阅，必须显式失败。
			if !validObserverEventKind(kind) {
				return fmt.Errorf("插件 %s 的观察器 %s 包含未知群事件类型 %q", s.Key, observer.Key, kind)
			}
			// [决策理由] 重复事件类型没有额外语义，通常表示声明错误。
			if _, found := seenKinds[kind]; found {
				return fmt.Errorf("插件 %s 的观察器 %s 重复声明群事件类型 %q", s.Key, observer.Key, kind)
			}
			seenKinds[kind] = struct{}{}
		}
	}

	// [决策理由] 配置契约错误会在管理页面首次读取时才暴露，必须在启动期拒绝。
	if err := s.validateConfig(); err != nil {
		return err
	}

	// >>> 数据演变示例
	// 1. echo+群命令+member角色+Handler -> 完整校验 -> nil。
	// 2. monitor+未知事件类型 -> 观察器校验 -> 返回错误。
	return nil
}

// validateConfig 校验可选的小型配置契约。
// @param 无。
// @returns Schema、字段类型或热应用钩子错误。
// ⚠️副作用说明：无。
func (s PluginSpec) validateConfig() error {
	// [决策理由] 不声明配置的插件由通用页面只管理开关，属于正常形态。
	if s.Config == nil {
		return nil
	}
	if err := s.Config.Schema.Validate(); err != nil {
		return fmt.Errorf("插件 %s 配置 Schema 无效: %w", s.Key, err)
	}
	// [决策理由] 空 Schema 会在管理页渲染出没有任何字段的配置表单。
	if len(s.Config.Schema.Fields) == 0 {
		return fmt.Errorf("插件 %s 声明配置但没有字段", s.Key)
	}
	// [决策理由] 无法热应用的配置只会写库不生效，属于不可解释状态。
	if s.Config.Apply == nil {
		return fmt.Errorf("插件 %s 配置缺少热应用钩子", s.Key)
	}
	for _, field := range s.Config.Schema.Fields {
		// [决策理由] 目标架构的小型配置只承载有限标量；会增长的结构化数据必须使用插件自有表与专属页面。
		if !validRuntimeConfigFieldType(field.Type) {
			return fmt.Errorf("插件 %s 的配置字段 %s 类型 %q 不属于小型配置", s.Key, field.Key, field.Type)
		}
	}

	// >>> 数据演变示例
	// 1. echo+string字段+Apply -> nil。
	// 2. monitor+weighted_terms_json -> 超出小型配置 -> 返回错误。
	return nil
}

// validRuntimeConfigFieldType 判断字段类型是否属于目标架构的小型配置集合。
// @param fieldType：待校验字段类型。
// @returns 属于有限标量类型时为 true。
// ⚠️副作用说明：无。
func validRuntimeConfigFieldType(fieldType FieldType) bool {
	switch fieldType {
	case FieldString, FieldMultiline, FieldInteger, FieldBoolean, FieldEnum, FieldSecret:
		return true
	default:
		return false
	}

	// >>> 数据演变示例
	// 1. string -> true。
	// 2. string_list_json -> false。
}

// validRole 判断身份是否属于目标架构封闭集合。
// @param role：待校验身份。
// @returns 已声明平台身份时为 true。
// ⚠️副作用说明：无。
func validRole(role Role) bool {
	result := role == RoleSuperAdmin || role == RoleGroupOwner || role == RoleGroupAdmin || role == RoleGroupMember

	// >>> 数据演变示例
	// 1. group_admin -> true。
	// 2. private_user -> false。
	return result
}

// validObserverEventKind 判断观察事件是否属于首版群事件白名单。
// @param kind：观察器声明的事件类别。
// @returns 支持的群消息、群通知或群请求时为 true。
// ⚠️副作用说明：无。
func validObserverEventKind(kind ObserverEventKind) bool {
	result := kind == ObserverGroupMessage || kind == ObserverGroupNotice || kind == ObserverGroupRequest

	// >>> 数据演变示例
	// 1. group_message -> true。
	// 2. private_message -> false。
	return result
}

// validObserverPhase 判断观察器阶段是否属于编译期声明集合。
func validObserverPhase(phase ObserverPhase) bool {
	return phase == ObserverPhaseAfterCommands || phase == ObserverPhaseBeforeCommands
}
