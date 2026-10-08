package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/w1ndys/w1ndys-bot/internal/ws"
)

func observerTestSpec(key string, kinds []ObserverEventKind, handler ObserverHandler) PluginSpec {
	return PluginSpec{
		Key:         key,
		DisplayName: key,
		Description: "观察群事件",
		Observers: []ObserverSpec{{
			Key:         "events",
			Description: "测试观察器",
			EventKinds:  kinds,
			Handler:     handler,
		}},
	}
}

func newObserverTestSubject(t *testing.T, specs ...PluginSpec) (*ObserverDispatcher, *RuntimeController) {
	t.Helper()
	catalog, err := NewSpecCatalog(specs)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewObserverDispatcher(catalog, controller)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher, controller
}

func TestObserverDispatcherRejectsNonGroupEvents(t *testing.T) {
	dispatcher, _ := newObserverTestSubject(t, observerTestSpec("observer", []ObserverEventKind{ObserverGroupMessage}, func(ObserverContext) error {
		t.Fatal("non-group event called observer")
		return nil
	}))
	tests := []ws.Event{
		&ws.MessageEvent{MessageType: "private", RawMessage: "hello"},
		&ws.NoticeEvent{GroupID: 0},
		&ws.GroupRequestEvent{RequestType: "group", GroupID: 0},
		&ws.OnlineFileNotice{NoticeEvent: ws.NoticeEvent{GroupID: 100}},
		&ws.BotOfflineNotice{NoticeEvent: ws.NoticeEvent{GroupID: 100}},
		&ws.HeartbeatEvent{},
		(*ws.MessageEvent)(nil),
		(*ws.NoticeEvent)(nil),
		(*ws.GroupRequestEvent)(nil),
	}
	for _, event := range tests {
		handled, err := dispatcher.Dispatch(context.Background(), event)
		if handled != 0 || err != nil {
			t.Fatalf("Dispatch(%T) = %d,%v", event, handled, err)
		}
	}
}

func TestObserverDispatcherAppliesReadyAndGroupGates(t *testing.T) {
	calls := 0
	dispatcher, controller := newObserverTestSubject(t, observerTestSpec("observer", []ObserverEventKind{ObserverGroupMessage}, func(ctx ObserverContext) error {
		calls++
		if ctx.GroupID != 100 {
			t.Fatalf("GroupID = %d", ctx.GroupID)
		}
		return nil
	}))
	event := &ws.MessageEvent{MessageType: "group", GroupID: 100}
	handled, err := dispatcher.Dispatch(context.Background(), event)
	if handled != 0 || err != nil {
		t.Fatalf("disabled Dispatch() = %d,%v", handled, err)
	}
	if err := controller.Enable(context.Background(), "observer"); err != nil {
		t.Fatal(err)
	}
	handled, err = dispatcher.Dispatch(context.Background(), event)
	if handled != 0 || err != nil {
		t.Fatalf("closed group Dispatch() = %d,%v", handled, err)
	}
	if err := controller.SetGroupEnabled("observer", 100, true); err != nil {
		t.Fatal(err)
	}
	handled, err = dispatcher.Dispatch(context.Background(), event)
	if handled != 1 || err != nil || calls != 1 {
		t.Fatalf("enabled Dispatch() = %d,%v calls=%d", handled, err, calls)
	}
	state, _ := controller.State("observer")
	if state.InFlight != 0 {
		t.Fatalf("inFlight = %d", state.InFlight)
	}
}

func TestObserverDispatcherRoutesOnlyDeclaredKinds(t *testing.T) {
	var received []ObserverEventKind
	spec := observerTestSpec("observer", []ObserverEventKind{ObserverGroupMessage, ObserverGroupRequest}, func(ctx ObserverContext) error {
		kind, _, _ := classifyObserverEvent(ctx.Event)
		received = append(received, kind)
		return nil
	})
	dispatcher, controller := newObserverTestSubject(t, spec)
	if err := controller.SetGroupEnabled("observer", 100, true); err != nil {
		t.Fatal(err)
	}
	if err := controller.Enable(context.Background(), "observer"); err != nil {
		t.Fatal(err)
	}
	events := []ws.Event{
		&ws.MessageEvent{MessageType: "group", GroupID: 100},
		&ws.NoticeEvent{GroupID: 100},
		&ws.GroupRequestEvent{RequestType: "group", GroupID: 100},
	}
	for _, event := range events {
		if _, err := dispatcher.Dispatch(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != 2 || received[0] != ObserverGroupMessage || received[1] != ObserverGroupRequest {
		t.Fatalf("received = %v", received)
	}
}

func TestObserverDispatcherRoutesOnlyDeclaredPhase(t *testing.T) {
	var calls []string
	before := observerTestSpec("before", []ObserverEventKind{ObserverGroupMessage}, func(ObserverContext) error {
		calls = append(calls, "before")
		return nil
	})
	before.Observers[0].Phase = ObserverPhaseBeforeCommands
	after := observerTestSpec("after", []ObserverEventKind{ObserverGroupMessage}, func(ObserverContext) error {
		calls = append(calls, "after")
		return nil
	})
	dispatcher, controller := newObserverTestSubject(t, before, after)
	for _, key := range []string{"before", "after"} {
		if err := controller.SetGroupEnabled(key, 100, true); err != nil {
			t.Fatal(err)
		}
		if err := controller.Enable(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	event := &ws.MessageEvent{MessageType: "group", GroupID: 100}
	handled, err := dispatcher.DispatchPhase(context.Background(), event, ObserverPhaseBeforeCommands)
	if handled != 1 || err != nil || len(calls) != 1 || calls[0] != "before" {
		t.Fatalf("before DispatchPhase() = %d,%v calls=%v", handled, err, calls)
	}
	handled, err = dispatcher.Dispatch(context.Background(), event)
	if handled != 1 || err != nil || len(calls) != 2 || calls[1] != "after" {
		t.Fatalf("default Dispatch() = %d,%v calls=%v", handled, err, calls)
	}
}

func TestObserverDispatcherReleasesAdmissionOnErrorAndPanic(t *testing.T) {
	handlerErr := errors.New("observer failed")
	t.Run("error", func(t *testing.T) {
		dispatcher, controller := newObserverTestSubject(t, observerTestSpec("observer", []ObserverEventKind{ObserverGroupMessage}, func(ObserverContext) error { return handlerErr }))
		_ = controller.SetGroupEnabled("observer", 100, true)
		_ = controller.Enable(context.Background(), "observer")
		handled, err := dispatcher.Dispatch(context.Background(), &ws.MessageEvent{MessageType: "group", GroupID: 100})
		if handled != 1 || !errors.Is(err, handlerErr) {
			t.Fatalf("Dispatch() = %d,%v", handled, err)
		}
		state, _ := controller.State("observer")
		if state.InFlight != 0 {
			t.Fatalf("inFlight = %d", state.InFlight)
		}
	})
	t.Run("panic", func(t *testing.T) {
		dispatcher, controller := newObserverTestSubject(t, observerTestSpec("observer", []ObserverEventKind{ObserverGroupMessage}, func(ObserverContext) error { panic("boom") }))
		_ = controller.SetGroupEnabled("observer", 100, true)
		_ = controller.Enable(context.Background(), "observer")
		panicked := false
		func() {
			defer func() { panicked = recover() != nil }()
			_, _ = dispatcher.Dispatch(context.Background(), &ws.MessageEvent{MessageType: "group", GroupID: 100})
		}()
		state, _ := controller.State("observer")
		if !panicked || state.InFlight != 0 {
			t.Fatalf("panicked=%t inFlight=%d", panicked, state.InFlight)
		}
	})
}

func TestClassifyObserverEventSupportsConcreteGroupNotices(t *testing.T) {
	base := ws.NoticeEvent{GroupID: 100}
	events := []ws.Event{
		&ws.NoticeEvent{GroupID: 100},
		&ws.GroupBanNotice{NoticeEvent: base},
		&ws.GroupCardNotice{NoticeEvent: base},
		&ws.GroupUploadNotice{NoticeEvent: base},
		&ws.EssenceNotice{NoticeEvent: base},
		&ws.EmojiLikeNotice{NoticeEvent: base},
		&ws.NotifyNotice{NoticeEvent: base},
	}
	for _, event := range events {
		kind, groupID, accepted := classifyObserverEvent(event)
		if !accepted || kind != ObserverGroupNotice || groupID != 100 {
			t.Errorf("classifyObserverEvent(%T) = %q,%d,%t", event, kind, groupID, accepted)
		}
	}
}

func TestObserverDispatcherContinuesAfterErrorsAndAggregatesThem(t *testing.T) {
	firstErr := errors.New("first failed")
	thirdErr := errors.New("third failed")
	var calls []string
	specs := []PluginSpec{
		observerTestSpec("a_first", []ObserverEventKind{ObserverGroupMessage}, func(ObserverContext) error {
			calls = append(calls, "first")
			return firstErr
		}),
		observerTestSpec("b_second", []ObserverEventKind{ObserverGroupMessage}, func(ObserverContext) error {
			calls = append(calls, "second")
			return nil
		}),
		observerTestSpec("c_third", []ObserverEventKind{ObserverGroupMessage}, func(ObserverContext) error {
			calls = append(calls, "third")
			return thirdErr
		}),
	}
	dispatcher, controller := newObserverTestSubject(t, specs...)
	for _, key := range []string{"a_first", "b_second", "c_third"} {
		if err := controller.SetGroupEnabled(key, 100, true); err != nil {
			t.Fatal(err)
		}
		if err := controller.Enable(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	handled, err := dispatcher.Dispatch(context.Background(), &ws.MessageEvent{MessageType: "group", GroupID: 100})
	if handled != 3 || !errors.Is(err, firstErr) || !errors.Is(err, thirdErr) {
		t.Fatalf("Dispatch() = %d,%v", handled, err)
	}
	wantCalls := []string{"first", "second", "third"}
	if len(calls) != len(wantCalls) {
		t.Fatalf("calls = %v", calls)
	}
	for index := range wantCalls {
		if calls[index] != wantCalls[index] {
			t.Fatalf("calls = %v", calls)
		}
	}
	for _, key := range []string{"a_first", "b_second", "c_third"} {
		state, _ := controller.State(key)
		if state.InFlight != 0 {
			t.Fatalf("%s inFlight = %d", key, state.InFlight)
		}
	}
}

func TestNewObserverDispatcherRejectsMissingDependencies(t *testing.T) {
	catalog, err := NewSpecCatalog([]PluginSpec{observerTestSpec("observer", []ObserverEventKind{ObserverGroupMessage}, func(ObserverContext) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	controller, _ := NewRuntimeController(catalog)
	tests := []struct {
		catalog *SpecCatalog
		gate    RuntimeGate
	}{
		{gate: controller},
		{catalog: catalog},
	}
	for _, test := range tests {
		dispatcher, err := NewObserverDispatcher(test.catalog, test.gate)
		if dispatcher != nil || err == nil {
			t.Fatalf("NewObserverDispatcher() = %v,%v", dispatcher, err)
		}
	}
}
