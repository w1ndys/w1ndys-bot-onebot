package plugin

import (
	"context"
	"errors"
	"fmt"

	"github.com/w1ndys/w1ndys-bot/internal/ws"
)

type observerRoute struct {
	pluginKey string
	observer  ObserverSpec
	kinds     map[ObserverEventKind]struct{}
}

// ObserverDispatcher 将群事件分发给通过运行门禁的代码观察器。
type ObserverDispatcher struct {
	routes []observerRoute
	gate   RuntimeGate
}

// NewObserverDispatcher 从规格目录建立纯内存观察路由。
func NewObserverDispatcher(catalog *SpecCatalog, gate RuntimeGate) (*ObserverDispatcher, error) {
	if catalog == nil {
		return nil, errors.New("观察器规格目录不能为空")
	}
	if gate == nil {
		return nil, errors.New("观察器 RuntimeGate 不能为空")
	}
	routes := make([]observerRoute, 0)
	for _, spec := range catalog.Specs() {
		for _, observer := range spec.Observers {
			kinds := make(map[ObserverEventKind]struct{}, len(observer.EventKinds))
			for _, kind := range observer.EventKinds {
				kinds[kind] = struct{}{}
			}
			routes = append(routes, observerRoute{pluginKey: spec.Key, observer: observer, kinds: kinds})
		}
	}
	return &ObserverDispatcher{routes: routes, gate: gate}, nil
}

// Dispatch 调用所有匹配且通过全局、群门禁的观察器。
func (d *ObserverDispatcher) Dispatch(ctx context.Context, event ws.Event) (int, error) {
	return d.DispatchPhase(ctx, event, ObserverPhaseAfterCommands)
}

// DispatchPhase 调用指定阶段中所有匹配且通过全局、群门禁的观察器。
func (d *ObserverDispatcher) DispatchPhase(ctx context.Context, event ws.Event, phase ObserverPhase) (int, error) {
	kind, groupID, accepted := classifyObserverEvent(event)
	if !accepted {
		return 0, nil
	}
	handled := 0
	var dispatchErrors []error
	for _, route := range d.routes {
		if route.observer.Phase != phase {
			continue
		}
		if _, subscribed := route.kinds[kind]; !subscribed {
			continue
		}
		admission, admitted := d.gate.Admit(route.pluginKey)
		if !admitted || admission == nil {
			continue
		}
		called, err := invokeObserver(ctx, admission, route.observer.Handler, groupID, event)
		if called {
			handled++
		}
		if err != nil {
			dispatchErrors = append(dispatchErrors, fmt.Errorf("观察器 %s.%s 处理事件: %w", route.pluginKey, route.observer.Key, err))
		}
	}
	return handled, errors.Join(dispatchErrors...)
}

func invokeObserver(ctx context.Context, admission Admission, handler ObserverHandler, groupID int64, event ws.Event) (bool, error) {
	defer admission.Release()
	if !admission.GroupEnabled(groupID) {
		return false, nil
	}
	return true, handler(ObserverContext{Context: ctx, GroupID: groupID, Event: event})
}

func classifyObserverEvent(event ws.Event) (ObserverEventKind, int64, bool) {
	var kind ObserverEventKind
	var groupID int64
	switch typed := event.(type) {
	case *ws.MessageEvent:
		if typed == nil || typed.MessageType != "group" {
			return "", 0, false
		}
		kind, groupID = ObserverGroupMessage, typed.GroupID
	case *ws.GroupRequestEvent:
		if typed == nil || typed.RequestType != "group" {
			return "", 0, false
		}
		kind, groupID = ObserverGroupRequest, typed.GroupID
	case *ws.NoticeEvent:
		if typed == nil {
			return "", 0, false
		}
		kind, groupID = ObserverGroupNotice, typed.GroupID
	case *ws.GroupBanNotice:
		if typed == nil {
			return "", 0, false
		}
		kind, groupID = ObserverGroupNotice, typed.GroupID
	case *ws.GroupCardNotice:
		if typed == nil {
			return "", 0, false
		}
		kind, groupID = ObserverGroupNotice, typed.GroupID
	case *ws.GroupUploadNotice:
		if typed == nil {
			return "", 0, false
		}
		kind, groupID = ObserverGroupNotice, typed.GroupID
	case *ws.EssenceNotice:
		if typed == nil {
			return "", 0, false
		}
		kind, groupID = ObserverGroupNotice, typed.GroupID
	case *ws.EmojiLikeNotice:
		if typed == nil {
			return "", 0, false
		}
		kind, groupID = ObserverGroupNotice, typed.GroupID
	case *ws.NotifyNotice:
		if typed == nil {
			return "", 0, false
		}
		kind, groupID = ObserverGroupNotice, typed.GroupID
	default:
		return "", 0, false
	}
	if groupID <= 0 {
		return "", 0, false
	}
	return kind, groupID, true
}
