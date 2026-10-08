// 📌 影响范围：验证目标插件规格、代码身份、群命令、群观察器和生命周期入口；无外部变量。
package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type specTestLifecycle struct{}

// OnEnable 实现测试生命周期启用契约。
// @param context.Context：未使用的测试上下文。
// @returns nil。
// ⚠️副作用说明：无。
func (specTestLifecycle) OnEnable(context.Context) error {
	// >>> 数据演变示例
	// 1. background启用 -> nil。
	// 2. 重复启用 -> nil。
	return nil
}

// OnDisable 实现测试生命周期禁用契约。
// @param context.Context：未使用的测试上下文。
// @returns nil。
// ⚠️副作用说明：无。
func (specTestLifecycle) OnDisable(context.Context) error {
	// >>> 数据演变示例
	// 1. background禁用 -> nil。
	// 2. 重复禁用 -> nil。
	return nil
}

// validCommandSpec 返回最小合法群命令规格。
// @param key：命令稳定 Key；trigger：触发词。
// @returns 允许群成员且使用空操作 Handler 的命令。
// ⚠️副作用说明：分配身份映射和触发词切片。
func validCommandSpec(key string, trigger string) CommandSpec {
	result := CommandSpec{Key: key, DisplayName: "测试命令", Description: "验证命令契约", Triggers: []string{trigger}, Scope: CommandScopeGroup, AllowedRoles: Roles(RoleGroupMember), Handler: func(CommandContext) error { return nil }}

	// >>> 数据演变示例
	// 1. echo+echo -> 合法群命令规格。
	// 2. rank+排行 -> 独立触发词和角色集合。
	return result
}

// validPluginSpec 返回包含单个群命令的合法插件规格。
// @param key：插件稳定 Key；trigger：命令触发词。
// @returns 可用于变异测试的插件规格。
// ⚠️副作用说明：分配命令及其集合。
func validPluginSpec(key string, trigger string) PluginSpec {
	result := PluginSpec{Key: key, DisplayName: "测试插件", Description: "验证目标插件规格", Commands: []CommandSpec{validCommandSpec("run", trigger)}}

	// >>> 数据演变示例
	// 1. echo+echo -> 单命令echo插件。
	// 2. tools+run -> 单命令tools插件。
	return result
}

// TestRolesContains 验证角色集合去重和显式授权。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：无。
func TestRolesContains(t *testing.T) {
	roles := Roles(RoleGroupMember, RoleGroupAdmin, RoleGroupMember)
	// [决策理由] 重复身份应折叠且保留显式成员授权。
	if len(roles) != 2 || !roles.Contains(RoleGroupMember) {
		t.Fatalf("Roles() = %#v", roles)
	}
	// [决策理由] 未声明的群主不能隐式通过较低身份授权。
	if roles.Contains(RoleGroupOwner) {
		t.Fatal("未声明身份被隐式允许")
	}

	// >>> 数据演变示例
	// 1. member,admin,member -> 两项集合 -> member允许。
	// 2. 集合无owner -> Contains(owner) -> false。
}

// TestPluginSpecValidateAcceptsEntryKinds 验证命令、观察器和纯后台入口均可独立注册。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：无。
func TestPluginSpecValidateAcceptsEntryKinds(t *testing.T) {
	specs := []PluginSpec{
		validPluginSpec("command_plugin", "command"),
		{Key: "observer_plugin", DisplayName: "观察插件", Description: "观察群消息", Observers: []ObserverSpec{{Key: "messages", Description: "处理未匹配群消息", EventKinds: []ObserverEventKind{ObserverGroupMessage}, Handler: func(ObserverContext) error { return nil }}}},
		{Key: "background_plugin", DisplayName: "后台插件", Description: "运行后台任务", Lifecycle: specTestLifecycle{}},
	}
	for _, spec := range specs {
		// [决策理由] 三类目标入口都必须能独立形成合法插件，不能要求伪造命令。
		if err := spec.Validate(); err != nil {
			t.Errorf("%s.Validate() error = %v", spec.Key, err)
		}
	}

	// >>> 数据演变示例
	// 1. command_plugin+群命令 -> 校验通过。
	// 2. background_plugin+Lifecycle -> 无伪命令仍校验通过。
}

// TestPluginSpecValidateRejectsInvalidCommands 验证命令标识、作用域、身份、Handler 和触发词边界。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：无。
func TestPluginSpecValidateRejectsInvalidCommands(t *testing.T) {
	base := validPluginSpec("command_test", "run")
	tests := []struct {
		name string
		edit func(*PluginSpec)
		want string
	}{
		{name: "空插件入口", edit: func(spec *PluginSpec) { spec.Commands = nil }, want: "至少需要命令"},
		{name: "非法插件Key", edit: func(spec *PluginSpec) { spec.Key = "Command-Test" }, want: "无效插件 Key"},
		{name: "非法页面Key", edit: func(spec *PluginSpec) { spec.AdminPageKey = "page/path" }, want: "管理页面 Key"},
		{name: "空命令说明", edit: func(spec *PluginSpec) { spec.Commands[0].Description = "" }, want: "展示名称或说明为空"},
		{name: "私聊作用域", edit: func(spec *PluginSpec) { spec.Commands[0].Scope = "private" }, want: "作用域"},
		{name: "空触发词集合", edit: func(spec *PluginSpec) { spec.Commands[0].Triggers = nil }, want: "至少需要一个触发词"},
		{name: "空身份集合", edit: func(spec *PluginSpec) { spec.Commands[0].AllowedRoles = nil }, want: "必须声明允许身份"},
		{name: "未知身份", edit: func(spec *PluginSpec) { spec.Commands[0].AllowedRoles = Roles("private_user") }, want: "未知身份"},
		{name: "空Handler", edit: func(spec *PluginSpec) { spec.Commands[0].Handler = nil }, want: "Handler 不能为空"},
		{name: "空白触发词", edit: func(spec *PluginSpec) { spec.Commands[0].Triggers = []string{"   "} }, want: "触发词"},
		{name: "重复触发词", edit: func(spec *PluginSpec) { spec.Commands[0].Triggers = []string{" RUN ", "run"} }, want: "触发词 \"run\" 重复"},
		{name: "重复入口Key", edit: func(spec *PluginSpec) { spec.Commands = append(spec.Commands, validCommandSpec("run", "other")) }, want: "入口 \"run\" 重复"},
	}
	for _, test := range tests {
		candidate := clonePluginSpec(base)
		test.edit(&candidate)
		err := candidate.Validate()
		// [决策理由] 每个非法命令边界都必须在 Catalog 构建前返回可定位错误。
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: Validate() error = %v, want contains %q", test.name, err, test.want)
		}
	}

	// >>> 数据演变示例
	// 1. private作用域 -> 目标仅群命令 -> 返回作用域错误。
	// 2. 未知private_user身份 -> 封闭角色校验 -> 返回未知身份错误。
}

// TestPluginSpecValidateRejectsInvalidObservers 验证观察器标识、事件和 Handler 边界。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：无。
func TestPluginSpecValidateRejectsInvalidObservers(t *testing.T) {
	base := PluginSpec{Key: "observer_test", DisplayName: "观察测试", Description: "验证观察器", Observers: []ObserverSpec{{Key: "events", Description: "观察群事件", EventKinds: []ObserverEventKind{ObserverGroupMessage}, Handler: func(ObserverContext) error { return nil }}}}
	tests := []struct {
		name string
		edit func(*PluginSpec)
		want string
	}{
		{name: "非法Key", edit: func(spec *PluginSpec) { spec.Observers[0].Key = "Events-1" }, want: "无效观察器 Key"},
		{name: "空说明", edit: func(spec *PluginSpec) { spec.Observers[0].Description = "" }, want: "说明为空"},
		{name: "未知阶段", edit: func(spec *PluginSpec) { spec.Observers[0].Phase = "during_commands" }, want: "未知执行阶段"},
		{name: "空事件", edit: func(spec *PluginSpec) { spec.Observers[0].EventKinds = nil }, want: "至少需要一个群事件类型"},
		{name: "未知事件", edit: func(spec *PluginSpec) { spec.Observers[0].EventKinds = []ObserverEventKind{"private_message"} }, want: "未知群事件类型"},
		{name: "重复事件", edit: func(spec *PluginSpec) {
			spec.Observers[0].EventKinds = []ObserverEventKind{ObserverGroupMessage, ObserverGroupMessage}
		}, want: "重复声明群事件类型"},
		{name: "空Handler", edit: func(spec *PluginSpec) { spec.Observers[0].Handler = nil }, want: "Handler 不能为空"},
		{name: "与命令同Key", edit: func(spec *PluginSpec) { spec.Commands = []CommandSpec{validCommandSpec("events", "events")} }, want: "入口 \"events\" 重复"},
	}
	for _, test := range tests {
		candidate := clonePluginSpec(base)
		test.edit(&candidate)
		err := candidate.Validate()
		// [决策理由] 非法观察声明不能静默形成未受门禁保护的事件订阅。
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: Validate() error = %v, want contains %q", test.name, err, test.want)
		}
	}

	// >>> 数据演变示例
	// 1. group_message+Handler -> 合法观察入口。
	// 2. private_message -> 不在群事件白名单 -> 返回错误。
}

func TestPluginSpecValidateAcceptsObserverPhases(t *testing.T) {
	for _, phase := range []ObserverPhase{ObserverPhaseAfterCommands, ObserverPhaseBeforeCommands} {
		spec := PluginSpec{Key: "observer_test", DisplayName: "观察测试", Description: "验证观察器阶段", Observers: []ObserverSpec{{Key: "events", Description: "观察群事件", Phase: phase, EventKinds: []ObserverEventKind{ObserverGroupMessage}, Handler: func(ObserverContext) error { return nil }}}}
		if err := spec.Validate(); err != nil {
			t.Errorf("phase %q: Validate() error = %v", phase, err)
		}
	}
}

func TestPluginSpecValidatesSmallConfigContract(t *testing.T) {
	applyHook := func(context.Context, json.RawMessage) error { return nil }
	stringField := ConfigField{Key: "response_prefix", DisplayName: "回复前缀", Type: FieldString}
	tests := []struct {
		name    string
		config  *ConfigSpec
		wantErr bool
	}{
		{name: "无配置", config: nil},
		{name: "标量字段", config: &ConfigSpec{Schema: ConfigSchema{Fields: []ConfigField{stringField}}, Apply: applyHook}},
		{name: "缺少热应用钩子", config: &ConfigSpec{Schema: ConfigSchema{Fields: []ConfigField{stringField}}}, wantErr: true},
		{name: "空字段集合", config: &ConfigSpec{Schema: ConfigSchema{}, Apply: applyHook}, wantErr: true},
		{name: "无效 Schema", config: &ConfigSpec{Schema: ConfigSchema{Fields: []ConfigField{{Key: "Bad-Key", DisplayName: "x", Type: FieldString}}}, Apply: applyHook}, wantErr: true},
		{
			name:    "结构化字段超出小型配置",
			config:  &ConfigSpec{Schema: ConfigSchema{Fields: []ConfigField{{Key: "terms", DisplayName: "词库", Type: FieldWeightedTermsJSON}}}, Apply: applyHook},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validPluginSpec("echo", "echo")
			spec.Config = test.config
			err := spec.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestSpecCatalogIsolatesConfigFields(t *testing.T) {
	spec := validPluginSpec("echo", "echo")
	options := []string{"a", "b"}
	spec.Config = &ConfigSpec{
		Schema: ConfigSchema{Fields: []ConfigField{{Key: "mode", DisplayName: "模式", Type: FieldEnum, Options: options}}},
		Apply:  func(context.Context, json.RawMessage) error { return nil },
	}
	catalog, err := NewSpecCatalog([]PluginSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	// 目录必须与调用方切片隔离，避免运行期被外部修改配置选项。
	options[0] = "mutated"
	spec.Config.Schema.Fields[0].DisplayName = "mutated"
	stored, found := catalog.Find("echo")
	if !found || stored.Config == nil {
		t.Fatalf("catalog config = %+v", stored.Config)
	}
	if stored.Config.Schema.Fields[0].Options[0] != "a" || stored.Config.Schema.Fields[0].DisplayName != "模式" {
		t.Fatalf("config not isolated: %+v", stored.Config.Schema.Fields[0])
	}
}
