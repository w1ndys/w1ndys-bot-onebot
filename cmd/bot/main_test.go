package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/w1ndys/w1ndys-bot/internal/plugin"
	"github.com/w1ndys/w1ndys-bot/internal/ws"
)

type fakeTargetEventDispatcher struct {
	calls     *[]string
	beforeErr error
	result    plugin.DispatchResult
}

func (f fakeTargetEventDispatcher) Dispatch(context.Context, ws.Event) (plugin.DispatchResult, error) {
	*f.calls = append(*f.calls, "full")
	return f.result, nil
}

func (f fakeTargetEventDispatcher) DispatchBeforeCommands(context.Context, ws.Event) (int, error) {
	*f.calls = append(*f.calls, "before")
	return 1, f.beforeErr
}

func (f fakeTargetEventDispatcher) DispatchAfterBeforeCommands(context.Context, ws.Event) (plugin.DispatchResult, error) {
	*f.calls = append(*f.calls, "after")
	return f.result, nil
}

type fakeEmergencyCommandHandler struct {
	calls   *[]string
	matched bool
	err     error
}

func (f fakeEmergencyCommandHandler) Handle(context.Context, *ws.MessageEvent, string) (bool, error) {
	*f.calls = append(*f.calls, "emergency")
	return f.matched, f.err
}

func TestDispatchBotEventChecksGroupEmergencyCommandsBeforeHandling(t *testing.T) {
	var calls []string
	target := fakeTargetEventDispatcher{calls: &calls}
	emergency := fakeEmergencyCommandHandler{calls: &calls, matched: true}
	event := &ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 200, RawMessage: "/启用当前群插件 echo"}
	if err := dispatchBotEvent(context.Background(), event, target, emergency, func(string) bool { return true }, "/"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"before", "emergency"}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestDispatchBotEventContinuesAfterUnmatchedEmergencyWithoutRepeatingBefore(t *testing.T) {
	var calls []string
	target := fakeTargetEventDispatcher{calls: &calls}
	emergency := fakeEmergencyCommandHandler{calls: &calls}
	event := &ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 200, RawMessage: "echo value"}
	if err := dispatchBotEvent(context.Background(), event, target, emergency, func(string) bool { return true }, "/"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"before", "emergency", "after"}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestDispatchBotEventUsesNormalChainForRegularGroupMessages(t *testing.T) {
	var calls []string
	target := fakeTargetEventDispatcher{calls: &calls}
	emergency := fakeEmergencyCommandHandler{calls: &calls}
	event := &ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 201, RawMessage: "echo value"}
	if err := dispatchBotEvent(context.Background(), event, target, emergency, func(string) bool { return false }, "/"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"full"}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestDispatchBotEventFailsClosedBeforeEmergencyCommand(t *testing.T) {
	var calls []string
	target := fakeTargetEventDispatcher{calls: &calls, beforeErr: errors.New("monitor unavailable")}
	emergency := fakeEmergencyCommandHandler{calls: &calls, matched: true}
	event := &ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 200, RawMessage: "/禁用当前群插件 echo"}
	if err := dispatchBotEvent(context.Background(), event, target, emergency, func(string) bool { return true }, "/"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"before"}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestDispatchBotEventKeepsPrivateEmergencyOutsideGroupPlugins(t *testing.T) {
	var calls []string
	target := fakeTargetEventDispatcher{calls: &calls}
	emergency := fakeEmergencyCommandHandler{calls: &calls, matched: true}
	event := &ws.MessageEvent{MessageType: "private", UserID: 200, RawMessage: "/插件列表"}
	if err := dispatchBotEvent(context.Background(), event, target, emergency, func(string) bool { return true }, "/"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"emergency"}) {
		t.Fatalf("calls = %v", calls)
	}
}
