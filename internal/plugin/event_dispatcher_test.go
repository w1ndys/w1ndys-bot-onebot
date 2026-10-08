package plugin

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/w1ndys/w1ndys-bot/internal/ws"
)

type eventDispatcherIdentity struct {
	role Role
}

func (r eventDispatcherIdentity) Resolve(context.Context, *ws.MessageEvent) (Role, error) {
	return r.role, nil
}

func newEventDispatcherTestSubject(t *testing.T, commandHandler CommandHandler, observerHandler ObserverHandler) (*EventDispatcher, *RuntimeController) {
	t.Helper()
	spec := PluginSpec{
		Key:         "combined",
		DisplayName: "统一分发测试",
		Description: "验证命令与观察器优先级",
		Commands: []CommandSpec{{
			Key:          "run",
			DisplayName:  "执行",
			Description:  "测试命令",
			Triggers:     []string{"run"},
			Scope:        CommandScopeGroup,
			AllowedRoles: Roles(RoleGroupMember),
			Handler:      commandHandler,
		}},
		Observers: []ObserverSpec{{
			Key:         "messages",
			Description: "观察未匹配群消息",
			EventKinds:  []ObserverEventKind{ObserverGroupMessage, ObserverGroupNotice, ObserverGroupRequest},
			Handler:     observerHandler,
		}},
	}
	catalog, err := NewSpecCatalog([]PluginSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := NewDispatcher(catalog, runtime, eventDispatcherIdentity{role: RoleGroupMember})
	if err != nil {
		t.Fatal(err)
	}
	observers, err := NewObserverDispatcher(catalog, runtime)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewEventDispatcher(commands, observers)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher, runtime
}

func newPhasedEventDispatcherTestSubject(t *testing.T, commandHandler CommandHandler, beforeHandler, afterHandler ObserverHandler) (*EventDispatcher, *RuntimeController) {
	t.Helper()
	spec := PluginSpec{
		Key:         "combined",
		DisplayName: "分阶段分发测试",
		Description: "验证命令前后观察器优先级",
		Commands:    []CommandSpec{{Key: "run", DisplayName: "执行", Description: "测试命令", Triggers: []string{"run"}, Scope: CommandScopeGroup, AllowedRoles: Roles(RoleGroupMember), Handler: commandHandler}},
		Observers: []ObserverSpec{
			{Key: "before", Description: "命令前观察", Phase: ObserverPhaseBeforeCommands, EventKinds: []ObserverEventKind{ObserverGroupMessage, ObserverGroupNotice}, Handler: beforeHandler},
			{Key: "after", Description: "命令后观察", EventKinds: []ObserverEventKind{ObserverGroupMessage, ObserverGroupNotice}, Handler: afterHandler},
		},
	}
	catalog, err := NewSpecCatalog([]PluginSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	commands, err := NewDispatcher(catalog, runtime, eventDispatcherIdentity{role: RoleGroupMember})
	if err != nil {
		t.Fatal(err)
	}
	observers, err := NewObserverDispatcher(catalog, runtime)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewEventDispatcher(commands, observers)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher, runtime
}

func TestEventDispatcherCommandMatchSuppressesObservers(t *testing.T) {
	commandCalls := 0
	observerCalls := 0
	dispatcher, runtime := newEventDispatcherTestSubject(t, func(CommandContext) error {
		commandCalls++
		return nil
	}, func(ObserverContext) error {
		observerCalls++
		return nil
	})
	if err := runtime.SetGroupEnabled("combined", 100, true); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Enable(context.Background(), "combined"); err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Dispatch(context.Background(), &ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 200, RawMessage: "run value"})
	if err != nil || !result.CommandMatched || result.ObserversHandled != 0 || commandCalls != 1 || observerCalls != 0 {
		t.Fatalf("Dispatch() = %+v,%v command=%d observer=%d", result, err, commandCalls, observerCalls)
	}
}

func TestEventDispatcherRunsBeforeObserverAheadOfMatchedCommand(t *testing.T) {
	var calls []string
	dispatcher, runtime := newPhasedEventDispatcherTestSubject(t, func(CommandContext) error {
		calls = append(calls, "command")
		return nil
	}, func(ObserverContext) error {
		calls = append(calls, "before")
		return nil
	}, func(ObserverContext) error {
		calls = append(calls, "after")
		return nil
	})
	if err := runtime.SetGroupEnabled("combined", 100, true); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Enable(context.Background(), "combined"); err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Dispatch(context.Background(), &ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 200, RawMessage: "run"})
	if err != nil || !result.CommandMatched || result.ObserversHandled != 1 || len(calls) != 2 || calls[0] != "before" || calls[1] != "command" {
		t.Fatalf("Dispatch() = %+v,%v calls=%v", result, err, calls)
	}
}

func TestEventDispatcherSplitDispatchDoesNotRepeatBeforeObserver(t *testing.T) {
	var calls []string
	dispatcher, runtime := newPhasedEventDispatcherTestSubject(t, func(CommandContext) error {
		calls = append(calls, "command")
		return nil
	}, func(ObserverContext) error {
		calls = append(calls, "before")
		return nil
	}, func(ObserverContext) error {
		calls = append(calls, "after")
		return nil
	})
	if err := runtime.SetGroupEnabled("combined", 100, true); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Enable(context.Background(), "combined"); err != nil {
		t.Fatal(err)
	}
	event := &ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 200, RawMessage: "run"}
	beforeHandled, err := dispatcher.DispatchBeforeCommands(context.Background(), event)
	if err != nil || beforeHandled != 1 {
		t.Fatalf("DispatchBeforeCommands() = %d,%v", beforeHandled, err)
	}
	result, err := dispatcher.DispatchAfterBeforeCommands(context.Background(), event)
	result.ObserversHandled += beforeHandled
	if err != nil || !result.CommandMatched || result.ObserversHandled != 1 || !reflect.DeepEqual(calls, []string{"before", "command"}) {
		t.Fatalf("split dispatch = %+v,%v calls=%v", result, err, calls)
	}
}

func TestEventDispatcherBeforeObserverErrorStopsCommandAndDefaultObservers(t *testing.T) {
	beforeErr := errors.New("before failed")
	commandCalls := 0
	afterCalls := 0
	dispatcher, runtime := newPhasedEventDispatcherTestSubject(t, func(CommandContext) error {
		commandCalls++
		return nil
	}, func(ObserverContext) error {
		return beforeErr
	}, func(ObserverContext) error {
		afterCalls++
		return nil
	})
	if err := runtime.SetGroupEnabled("combined", 100, true); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Enable(context.Background(), "combined"); err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Dispatch(context.Background(), &ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 200, RawMessage: "run"})
	if !errors.Is(err, beforeErr) || result.CommandMatched || result.ObserversHandled != 1 || commandCalls != 0 || afterCalls != 0 {
		t.Fatalf("Dispatch() = %+v,%v command=%d after=%d", result, err, commandCalls, afterCalls)
	}
}

func TestEventDispatcherRunsBothObserverPhasesForUnmatchedGroupEvents(t *testing.T) {
	var calls []string
	dispatcher, runtime := newPhasedEventDispatcherTestSubject(t, func(CommandContext) error { return nil }, func(ObserverContext) error {
		calls = append(calls, "before")
		return nil
	}, func(ObserverContext) error {
		calls = append(calls, "after")
		return nil
	})
	if err := runtime.SetGroupEnabled("combined", 100, true); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Enable(context.Background(), "combined"); err != nil {
		t.Fatal(err)
	}
	for _, event := range []ws.Event{
		&ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 200, RawMessage: "unmatched"},
		&ws.NoticeEvent{GroupID: 100},
	} {
		calls = nil
		result, err := dispatcher.Dispatch(context.Background(), event)
		if err != nil || result.CommandMatched || result.ObserversHandled != 2 || len(calls) != 2 || calls[0] != "before" || calls[1] != "after" {
			t.Fatalf("Dispatch(%T) = %+v,%v calls=%v", event, result, err, calls)
		}
	}
}

func TestEventDispatcherMatchedRejectionDoesNotFallThrough(t *testing.T) {
	observerCalls := 0
	dispatcher, _ := newEventDispatcherTestSubject(t, func(CommandContext) error {
		t.Fatal("disabled command called")
		return nil
	}, func(ObserverContext) error {
		observerCalls++
		return nil
	})
	result, err := dispatcher.Dispatch(context.Background(), &ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 200, RawMessage: "run"})
	if !errors.Is(err, ErrPluginNotReady) || !result.CommandMatched || observerCalls != 0 {
		t.Fatalf("Dispatch() = %+v,%v observer=%d", result, err, observerCalls)
	}
}

func TestEventDispatcherHandlerErrorDoesNotFallThrough(t *testing.T) {
	handlerErr := errors.New("command failed")
	observerCalls := 0
	dispatcher, runtime := newEventDispatcherTestSubject(t, func(CommandContext) error { return handlerErr }, func(ObserverContext) error {
		observerCalls++
		return nil
	})
	if err := runtime.SetGroupEnabled("combined", 100, true); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Enable(context.Background(), "combined"); err != nil {
		t.Fatal(err)
	}
	result, err := dispatcher.Dispatch(context.Background(), &ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 200, RawMessage: "run"})
	if !errors.Is(err, handlerErr) || !result.CommandMatched || result.ObserversHandled != 0 || observerCalls != 0 {
		t.Fatalf("Dispatch() = %+v,%v observer=%d", result, err, observerCalls)
	}
}

func TestEventDispatcherSendsUnmatchedMessagesAndNoticesToObservers(t *testing.T) {
	var events []ws.Event
	dispatcher, runtime := newEventDispatcherTestSubject(t, func(CommandContext) error { return nil }, func(ctx ObserverContext) error {
		events = append(events, ctx.Event)
		return nil
	})
	if err := runtime.SetGroupEnabled("combined", 100, true); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Enable(context.Background(), "combined"); err != nil {
		t.Fatal(err)
	}
	inputs := []ws.Event{
		&ws.MessageEvent{MessageType: "group", GroupID: 100, UserID: 200, RawMessage: "unmatched"},
		&ws.NoticeEvent{GroupID: 100},
		&ws.GroupRequestEvent{RequestType: "group", GroupID: 100},
	}
	for _, event := range inputs {
		result, err := dispatcher.Dispatch(context.Background(), event)
		if err != nil || result.CommandMatched || result.ObserversHandled != 1 {
			t.Fatalf("Dispatch(%T) = %+v,%v", event, result, err)
		}
	}
	if len(events) != 3 {
		t.Fatalf("observer events = %d", len(events))
	}
}

func TestEventDispatcherHandlesTypedNilAndUnsupportedEvents(t *testing.T) {
	dispatcher, _ := newEventDispatcherTestSubject(t, func(CommandContext) error { return nil }, func(ObserverContext) error {
		t.Fatal("invalid event called observer")
		return nil
	})
	var message *ws.MessageEvent
	result, err := dispatcher.Dispatch(context.Background(), message)
	if !errors.Is(err, ErrGroupMessageRequired) || result != (DispatchResult{}) {
		t.Fatalf("typed nil Dispatch() = %+v,%v", result, err)
	}
	result, err = dispatcher.Dispatch(context.Background(), &ws.HeartbeatEvent{})
	if err != nil || result != (DispatchResult{}) {
		t.Fatalf("heartbeat Dispatch() = %+v,%v", result, err)
	}
}

func TestEventDispatcherRejectsPrivateBeforeObservers(t *testing.T) {
	dispatcher, _ := newEventDispatcherTestSubject(t, func(CommandContext) error { return nil }, func(ObserverContext) error {
		t.Fatal("private message called observer")
		return nil
	})
	result, err := dispatcher.Dispatch(context.Background(), &ws.MessageEvent{MessageType: "private", UserID: 200, RawMessage: "unmatched"})
	if !errors.Is(err, ErrGroupMessageRequired) || result != (DispatchResult{}) {
		t.Fatalf("Dispatch() = %+v,%v", result, err)
	}
}

func TestNewEventDispatcherRejectsMissingDependencies(t *testing.T) {
	tests := []struct {
		commands  *Dispatcher
		observers *ObserverDispatcher
	}{
		{},
		{commands: &Dispatcher{}},
		{observers: &ObserverDispatcher{}},
	}
	for _, test := range tests {
		dispatcher, err := NewEventDispatcher(test.commands, test.observers)
		if dispatcher != nil || err == nil {
			t.Fatalf("NewEventDispatcher() = %v,%v", dispatcher, err)
		}
	}
}
