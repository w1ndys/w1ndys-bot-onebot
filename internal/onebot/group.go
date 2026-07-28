// 📌 影响范围：通过注入的 ActionCaller 调用 NapCat 群管理与群成员接口；可修改群成员禁言状态。
package onebot

import (
	"context"
	"fmt"
)

type getGroupListParams struct {
	NoCache bool `json:"no_cache"`
}

// GetGroupList 获取机器人加入的群列表。
func (a *API) GetGroupList(ctx context.Context, noCache bool) ([]GroupInfo, error) {
	var result []GroupInfo
	if err := a.Call(ctx, ActionGetGroupList, getGroupListParams{NoCache: noCache}, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("群列表响应不能为 null")
	}

	seen := make(map[int64]struct{}, len(result))
	for i, group := range result {
		if group.GroupID <= 0 {
			return nil, fmt.Errorf("群列表第 %d 项的群号无效: %d", i, group.GroupID)
		}
		if _, exists := seen[group.GroupID]; exists {
			return nil, fmt.Errorf("群列表包含重复群号: %d", group.GroupID)
		}
		seen[group.GroupID] = struct{}{}
	}

	return result, nil
}

// SetGroupBan 设置或解除指定群成员的禁言。
// @param ctx：请求上下文；params：群号、用户 QQ 及秒数或字符串形式的禁言时长。
// @returns 参数、Action 业务或传输错误。
// ⚠️副作用说明：调用 NapCat set_group_ban；duration=0 会解除禁言，正数会设置禁言。
func (a *API) SetGroupBan(ctx context.Context, params SetGroupBanParams) error {
	// [决策理由] 群号和 QQ 号是 OpenAPI 必填字符串，空值无法定位目标。
	if params.GroupID == "" || params.UserID == "" {
		return fmt.Errorf("群号和用户 QQ 不能为空")
	}
	// [决策理由] NapCat 4.18.13 仅接受 number|string 形式的 duration。
	if !validStringOrNumber(params.Duration) {
		return fmt.Errorf("禁言时长必须为字符串或数字")
	}
	err := a.Call(ctx, ActionSetGroupBan, params, nil)

	// >>> 数据演变示例
	// 1. {group_id:100,user_id:200,duration:2592000} -> set_group_ban -> 禁言30天。
	// 2. {group_id:100,user_id:200,duration:0} -> set_group_ban -> 解除禁言。
	return err
}

// GetGroupMemberListParams 表示群成员列表查询参数。
type GetGroupMemberListParams struct {
	GroupID string `json:"group_id"`
	NoCache bool   `json:"no_cache,omitempty"`
}

// GetGroupMemberList 获取指定群的成员列表。
// @param ctx：请求上下文；params：目标群号与是否跳过 NapCat 缓存。
// @returns 完整的常用群成员字段，或参数、Action、业务与解析错误。
// ⚠️副作用说明：通过 NapCat 读取群成员数据，可能使用服务端缓存。
func (a *API) GetGroupMemberList(ctx context.Context, params GetGroupMemberListParams) ([]GroupMemberInfo, error) {
	// [决策理由] OpenAPI 要求 group_id 为必填字符串。
	if params.GroupID == "" {
		return nil, fmt.Errorf("群号不能为空")
	}
	var result []GroupMemberInfo
	err := a.Call(ctx, ActionGetGroupMemberList, params, &result)

	// >>> 数据演变示例
	// 1. {group_id:100,no_cache:true} -> data[{user_id:200,join_time:1}] -> []GroupMemberInfo{{UserID:200,JoinTime:1}}。
	// 2. {group_id:""} -> 本地参数错误 -> 不调用 NapCat。
	return result, err
}
