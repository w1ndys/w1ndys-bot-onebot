// 📌 影响范围：使用内存 fake ActionCaller 验证群管理、群成员和群历史消息接口；不访问真实 QQ。
package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/w1ndys/w1ndys-bot/internal/ws"
)

func TestGetGroupListPreservesFieldsAndParams(t *testing.T) {
	data := `[{"group_id":100,"group_name":"测试群","group_remark":"备注","group_all_shut":1,"member_count":20,"max_member_count":200}]`
	for _, test := range []struct {
		name    string
		noCache bool
		want    string
	}{
		{name: "skip cache", noCache: true, want: `{"no_cache":true}`},
		{name: "use cache", noCache: false, want: `{"no_cache":false}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			caller := &fakeCaller{result: ws.ActionResponse{Status: "ok", Data: json.RawMessage(data)}}
			result, err := New(caller).GetGroupList(context.Background(), test.noCache)
			if err != nil {
				t.Fatalf("GetGroupList() error=%v", err)
			}
			if len(result) != 1 || result[0].GroupID != 100 || result[0].GroupName != "测试群" || result[0].GroupRemark != "备注" || result[0].GroupAllShut != 1 || result[0].MemberCount != 20 || result[0].MaxMemberCount != 200 {
				t.Fatalf("GetGroupList() result=%+v", result)
			}
			if caller.action != string(ActionGetGroupList) {
				t.Fatalf("GetGroupList() action=%q", caller.action)
			}
			params, marshalErr := json.Marshal(caller.params)
			if marshalErr != nil {
				t.Fatalf("marshal params: %v", marshalErr)
			}
			if string(params) != test.want {
				t.Fatalf("GetGroupList() params=%s want=%s", params, test.want)
			}
		})
	}
}

func TestGetGroupListRejectsInvalidIDs(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
	}{
		{name: "zero", data: `[{"group_id":0}]`},
		{name: "negative", data: `[{"group_id":-1}]`},
		{name: "duplicate", data: `[{"group_id":100},{"group_id":100}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			caller := &fakeCaller{result: ws.ActionResponse{Status: "ok", Data: json.RawMessage(test.data)}}
			result, err := New(caller).GetGroupList(context.Background(), false)
			if err == nil || result != nil {
				t.Fatalf("GetGroupList() result=%+v error=%v", result, err)
			}
		})
	}
}

func TestGetGroupListDistinguishesEmptyArrayFromNull(t *testing.T) {
	empty, err := New(&fakeCaller{result: ws.ActionResponse{Status: "ok", Data: json.RawMessage(`[]`)}}).GetGroupList(context.Background(), true)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty array result=%#v error=%v", empty, err)
	}
	nullResult, err := New(&fakeCaller{result: ws.ActionResponse{Status: "ok", Data: json.RawMessage(`null`)}}).GetGroupList(context.Background(), true)
	if err == nil || nullResult != nil {
		t.Fatalf("null result=%#v error=%v", nullResult, err)
	}
}

func TestGetGroupListDiscardsResultsOnCallErrors(t *testing.T) {
	wantErr := errors.New("connection closed")
	for _, test := range []struct {
		name   string
		caller *fakeCaller
	}{
		{
			name:   "caller error",
			caller: &fakeCaller{err: wantErr},
		},
		{
			name:   "action error",
			caller: &fakeCaller{result: ws.ActionResponse{Status: "failed", RetCode: 1400, Message: "bad request"}},
		},
		{
			name:   "decode error",
			caller: &fakeCaller{result: ws.ActionResponse{Status: "ok", Data: json.RawMessage(`[{"group_id":100},{"group_id":"bad"}]`)}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := New(test.caller).GetGroupList(context.Background(), false)
			if err == nil || result != nil {
				t.Fatalf("GetGroupList() result=%+v error=%v", result, err)
			}
		})
	}
}

// TestSetGroupBanValidatesAndCallsOfficialAction 验证禁言参数校验与官方 Action 请求形状。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：修改 fakeCaller 记录的请求。
func TestSetGroupBanValidatesAndCallsOfficialAction(t *testing.T) {
	caller := &fakeCaller{result: ws.ActionResponse{Status: "ok", Data: json.RawMessage(`null`)}}
	api := New(caller)
	err := api.SetGroupBan(context.Background(), SetGroupBanParams{GroupID: "100", UserID: "200", Duration: 2592000})
	// [决策理由] 合法请求必须选择 set_group_ban 并完整保留30天秒数。
	if err != nil || caller.action != string(ActionSetGroupBan) {
		t.Fatalf("SetGroupBan() action=%q err=%v", caller.action, err)
	}
	params, ok := caller.params.(SetGroupBanParams)
	// [决策理由] 强类型参数应直接传给 ActionCaller，避免字段名漂移。
	if !ok || params.Duration != 2592000 {
		t.Fatalf("SetGroupBan() params=%T %+v", caller.params, caller.params)
	}
	caller.action = ""
	err = api.SetGroupBan(context.Background(), SetGroupBanParams{GroupID: "100", UserID: "200", Duration: []int{1}})
	// [决策理由] OpenAPI 不允许数组 duration，必须在发送前拒绝。
	if err == nil || caller.action != "" {
		t.Fatalf("invalid SetGroupBan() action=%q err=%v", caller.action, err)
	}

	// >>> 数据演变示例
	// 1. duration=2592000 -> set_group_ban -> fake记录完整参数。
	// 2. duration=[]int{1} -> 本地校验失败 -> fake不记录Action。
}

// TestGetGroupMemberListPreservesWhitelistFields 验证白名单计算所需的成员字段不丢失。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：修改 fakeCaller 记录的请求。
func TestGetGroupMemberListPreservesWhitelistFields(t *testing.T) {
	data := `[{"group_id":100,"user_id":200,"join_time":123,"last_sent_time":456,"role":"admin","is_robot":true}]`
	caller := &fakeCaller{result: ws.ActionResponse{Status: "ok", Data: json.RawMessage(data)}}
	result, err := New(caller).GetGroupMemberList(context.Background(), GetGroupMemberListParams{GroupID: "100", NoCache: true})
	// [决策理由] 入群时间、身份和机器人标记是白名单与排除规则的必需输入。
	if err != nil || len(result) != 1 || result[0].JoinTime != 123 || result[0].Role != "admin" || !result[0].IsRobot {
		t.Fatalf("GetGroupMemberList() result=%+v err=%v", result, err)
	}
	// [决策理由] 群成员查询必须使用官方 Action 名称。
	if caller.action != string(ActionGetGroupMemberList) {
		t.Fatalf("GetGroupMemberList() action=%q", caller.action)
	}
	params, ok := caller.params.(GetGroupMemberListParams)
	// [决策理由] 每日白名单刷新需能显式跳过 NapCat 缓存，避免使用过期的入群信息。
	if !ok || !params.NoCache {
		t.Fatalf("GetGroupMemberList() params=%T %+v", caller.params, caller.params)
	}

	// >>> 数据演变示例
	// 1. join_time=123,role=admin,is_robot=true -> GroupMemberInfo 保留三字段。
	// 2. data仅一成员 -> result长度1 -> 可直接用于白名单刷新。
}

// TestGetGroupMessageHistoryBoundariesAndFields 验证页大小边界与撤回筛选所需字段。
// @param t：Go 测试上下文。
// @returns 无。
// ⚠️副作用说明：修改 fakeCaller 记录的请求。
func TestGetGroupMessageHistoryBoundariesAndFields(t *testing.T) {
	data := `{"messages":[{"time":111,"message_id":222,"user_id":333,"raw_message":"广告原文","sender":{"user_id":333,"role":"member"},"message":[{"type":"text"}]}]}`
	caller := &fakeCaller{result: ws.ActionResponse{Status: "ok", Data: json.RawMessage(data)}}
	api := New(caller)
	result, err := api.GetGroupMessageHistory(context.Background(), GetGroupMessageHistoryParams{GroupID: "100", MessageSeq: "222", Count: 30, ParseMultMsg: true})
	// [决策理由] 审计和撤回逻辑必须同时获得消息ID、发送者、时间、原文和 sender 载荷。
	if err != nil || len(result.Messages) != 1 {
		t.Fatalf("GetGroupMessageHistory() result=%+v err=%v", result, err)
	}
	message := result.Messages[0]
	// [决策理由] 任一关键字段丢失都会导致时间锚定、发送者筛选或撤回失效。
	if message.Time != 111 || message.MessageID != 222 || message.UserID != 333 || message.RawMessage != "广告原文" || len(message.Sender) == 0 || len(message.Message) == 0 {
		t.Fatalf("history message=%+v", message)
	}
	for _, count := range []int{0, 101} {
		caller.action = ""
		_, boundaryErr := api.GetGroupMessageHistory(context.Background(), GetGroupMessageHistoryParams{GroupID: "100", Count: count})
		// [决策理由] 1..100 之外的请求不应占用 ActionClient pending 资源。
		if boundaryErr == nil || caller.action != "" {
			t.Fatalf("count=%d action=%q err=%v", count, caller.action, boundaryErr)
		}
	}

	// >>> 数据演变示例
	// 1. count=30 + 一条消息 -> 关键字段完整解码 -> 可筛选撤回。
	// 2. count=0/101 -> 边界校验失败 -> 不调用 NapCat。
}
