package plugin

import (
	"context"
	"errors"

	"github.com/w1ndys/w1ndys-bot/internal/ws"
)

// DispatchResult 描述统一事件入口实际选择的执行分支。
type DispatchResult struct {
	CommandMatched   bool
	ObserversHandled int
}

// EventDispatcher 编排命令前观察、命令匹配和未匹配事件观察的唯一内存入口。
type EventDispatcher struct {
	commands  *Dispatcher
	observers *ObserverDispatcher
}

// NewEventDispatcher 创建命令和观察器共享的统一事件入口。
func NewEventDispatcher(commands *Dispatcher, observers *ObserverDispatcher) (*EventDispatcher, error) {
	if commands == nil {
		return nil, errors.New("命令 Dispatcher 不能为空")
	}
	if observers == nil {
		return nil, errors.New("观察器 Dispatcher 不能为空")
	}
	return &EventDispatcher{commands: commands, observers: observers}, nil
}

// Dispatch 先执行命令前观察器，再匹配群命令，并将未匹配事件交给默认观察器。
func (d *EventDispatcher) Dispatch(ctx context.Context, event ws.Event) (DispatchResult, error) {
	beforeHandled, err := d.DispatchBeforeCommands(ctx, event)
	result := DispatchResult{ObserversHandled: beforeHandled}
	if err != nil {
		return result, err
	}
	afterResult, err := d.DispatchAfterBeforeCommands(ctx, event)
	afterResult.ObserversHandled += beforeHandled
	return afterResult, err
}

// DispatchBeforeCommands 只执行命令前观察器，供更高层命令入口复用安全检查。
func (d *EventDispatcher) DispatchBeforeCommands(ctx context.Context, event ws.Event) (int, error) {
	return d.observers.DispatchPhase(ctx, event, ObserverPhaseBeforeCommands)
}

// DispatchAfterBeforeCommands 在调用方已完成命令前观察后继续命令和默认观察器链。
func (d *EventDispatcher) DispatchAfterBeforeCommands(ctx context.Context, event ws.Event) (DispatchResult, error) {
	result := DispatchResult{}
	if message, isMessage := event.(*ws.MessageEvent); isMessage {
		matched, err := d.commands.Dispatch(ctx, message)
		result.CommandMatched = matched
		if matched || err != nil {
			return result, err
		}
	}
	afterHandled, err := d.observers.Dispatch(ctx, event)
	result.ObserversHandled += afterHandled
	return result, err
}
