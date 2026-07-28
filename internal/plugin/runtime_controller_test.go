package plugin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type runtimeTestLifecycle struct {
	mu            sync.Mutex
	enableStarted chan struct{}
	enableBlock   chan struct{}
	disableCalled chan struct{}
	enableErr     error
	disableErr    error
	panicEnable   bool
	panicDisable  bool
	enableCalls   int
	disableCalls  int
}

func (l *runtimeTestLifecycle) OnEnable(context.Context) error {
	l.mu.Lock()
	l.enableCalls++
	started := l.enableStarted
	block := l.enableBlock
	shouldPanic := l.panicEnable
	err := l.enableErr
	l.mu.Unlock()
	if started != nil {
		close(started)
	}
	if block != nil {
		<-block
	}
	if shouldPanic {
		panic("enable panic")
	}
	return err
}

func (l *runtimeTestLifecycle) OnDisable(context.Context) error {
	l.mu.Lock()
	l.disableCalls++
	called := l.disableCalled
	shouldPanic := l.panicDisable
	err := l.disableErr
	l.mu.Unlock()
	if called != nil {
		close(called)
	}
	if shouldPanic {
		panic("disable panic")
	}
	return err
}

func newRuntimeTestController(t *testing.T, lifecycle Lifecycle) *RuntimeController {
	t.Helper()
	spec := validPluginSpec("runtime_test", "run")
	spec.Lifecycle = lifecycle
	catalog, err := NewSpecCatalog([]PluginSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func TestRuntimeControllerDefaultsFailClosed(t *testing.T) {
	controller := newRuntimeTestController(t, nil)
	state, found := controller.State("runtime_test")
	if !found || state.Status != RuntimeDisabled || state.InFlight != 0 || state.LastError != nil {
		t.Fatalf("initial State() = %+v,%t", state, found)
	}
	if admission, admitted := controller.Admit("runtime_test"); admitted || admission != nil {
		t.Fatal("disabled plugin admitted")
	}
	if admission, admitted := controller.Admit("missing"); admitted || admission != nil {
		t.Fatal("missing plugin admitted")
	}
	if _, found := controller.State("missing"); found {
		t.Fatal("missing plugin state found")
	}
}

func TestRuntimeControllerBecomesReadyAfterEnablePreparation(t *testing.T) {
	lifecycle := &runtimeTestLifecycle{enableStarted: make(chan struct{}), enableBlock: make(chan struct{})}
	controller := newRuntimeTestController(t, lifecycle)
	result := make(chan error, 1)
	go func() { result <- controller.Enable(context.Background(), "runtime_test") }()
	<-lifecycle.enableStarted
	state, _ := controller.State("runtime_test")
	if state.Status != RuntimeEnabling {
		t.Fatalf("State during OnEnable = %+v", state)
	}
	if admission, admitted := controller.Admit("runtime_test"); admitted || admission != nil {
		t.Fatal("enabling plugin admitted")
	}
	close(lifecycle.enableBlock)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	state, _ = controller.State("runtime_test")
	if state.Status != RuntimeReady {
		t.Fatalf("State after OnEnable = %+v", state)
	}
	if err := controller.Enable(context.Background(), "runtime_test"); err != nil {
		t.Fatalf("repeated Enable() error = %v", err)
	}
	if lifecycle.enableCalls != 1 {
		t.Fatalf("enable calls = %d", lifecycle.enableCalls)
	}
}

func TestRuntimeControllerGroupGateFailsClosedAndPersistsOffline(t *testing.T) {
	controller := newRuntimeTestController(t, nil)
	if err := controller.SetGroupEnabled("runtime_test", 100, true); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetGroupEnabled("runtime_test", 0, true); !errors.Is(err, ErrInvalidRuntimeGroupID) {
		t.Fatalf("invalid group error = %v", err)
	}
	if err := controller.SetGroupEnabled("missing", 100, true); !errors.Is(err, ErrRuntimePluginNotFound) {
		t.Fatalf("missing plugin error = %v", err)
	}
	if err := controller.Enable(context.Background(), "runtime_test"); err != nil {
		t.Fatal(err)
	}
	admission, admitted := controller.Admit("runtime_test")
	if !admitted || admission == nil {
		t.Fatal("ready plugin not admitted")
	}
	if !admission.GroupEnabled(100) || admission.GroupEnabled(200) || admission.GroupEnabled(0) {
		t.Fatal("group gate did not fail closed")
	}
	admission.Release()
	if admission.GroupEnabled(100) {
		t.Fatal("released admission retained group access")
	}
	admission.Release()
	state, _ := controller.State("runtime_test")
	if state.InFlight != 0 {
		t.Fatalf("idempotent Release inFlight = %d", state.InFlight)
	}
}

func TestRuntimeControllerBatchGroupStatesPrevalidateBeforeMutation(t *testing.T) {
	catalog, err := NewSpecCatalog([]PluginSpec{validPluginSpec("echo", "echo"), validPluginSpec("tools", "tools")})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.SetGroupStates(100, map[string]bool{"echo": true, "tools": true}); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetGroupStates(100, map[string]bool{"echo": false, "missing": false}); !errors.Is(err, ErrRuntimePluginNotFound) {
		t.Fatalf("SetGroupStates() error = %v", err)
	}
	for _, pluginKey := range []string{"echo", "tools"} {
		if enabled, found := runtimeControllerGroupState(t, controller, pluginKey, 100); !found || !enabled {
			t.Fatalf("prevalidation mutated %s = %t,%t", pluginKey, enabled, found)
		}
	}
	if err := controller.SetGroupStates(100, map[string]bool{"tools": false, "echo": false}); err != nil {
		t.Fatal(err)
	}
	duplicate := []RuntimeGroupStateKey{{PluginKey: "echo", GroupID: 100}, {PluginKey: "echo", GroupID: 100}}
	if err := controller.DeleteGroupStates(duplicate); !errors.Is(err, ErrRuntimeGroupStateDuplicate) {
		t.Fatalf("DeleteGroupStates() duplicate error = %v", err)
	}
	if _, found := runtimeControllerGroupState(t, controller, "echo", 100); !found {
		t.Fatal("duplicate cleanup partially deleted state")
	}
	if err := controller.DeleteGroupStates([]RuntimeGroupStateKey{{PluginKey: "tools", GroupID: 100}, {PluginKey: "echo", GroupID: 100}}); err != nil {
		t.Fatal(err)
	}
	for _, pluginKey := range []string{"echo", "tools"} {
		if _, found := runtimeControllerGroupState(t, controller, pluginKey, 100); found {
			t.Fatalf("state %s was not deleted", pluginKey)
		}
	}
}

func TestRuntimeControllerBatchGroupStatesConcurrentWithAdmission(t *testing.T) {
	catalog, err := NewSpecCatalog([]PluginSpec{validPluginSpec("echo", "echo"), validPluginSpec("tools", "tools")})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewRuntimeController(catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, pluginKey := range []string{"echo", "tools"} {
		if err := controller.Enable(context.Background(), pluginKey); err != nil {
			t.Fatal(err)
		}
	}
	keys := []RuntimeGroupStateKey{{PluginKey: "echo", GroupID: 100}, {PluginKey: "tools", GroupID: 100}}
	errorsFound := make(chan error, 4)
	var group sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for index := 0; index < 1000; index++ {
				switch worker {
				case 0:
					if err := controller.SetGroupStates(100, map[string]bool{"echo": index%2 == 0, "tools": index%2 != 0}); err != nil {
						errorsFound <- err
						return
					}
				case 1:
					if err := controller.DeleteGroupStates(keys); err != nil {
						errorsFound <- err
						return
					}
				default:
					for _, pluginKey := range []string{"echo", "tools"} {
						admission, admitted := controller.Admit(pluginKey)
						if admitted {
							_ = admission.GroupEnabled(100)
							admission.Release()
						}
					}
				}
			}
		}(worker)
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatalf("concurrent batch operation: %v", err)
	}
}

func TestRuntimeControllerDisableStopsAdmissionAndDrains(t *testing.T) {
	lifecycle := &runtimeTestLifecycle{disableCalled: make(chan struct{})}
	controller := newRuntimeTestController(t, lifecycle)
	if err := controller.SetGroupEnabled("runtime_test", 100, true); err != nil {
		t.Fatal(err)
	}
	if err := controller.Enable(context.Background(), "runtime_test"); err != nil {
		t.Fatal(err)
	}
	admission, admitted := controller.Admit("runtime_test")
	if !admitted {
		t.Fatal("ready plugin not admitted")
	}
	disableResult := make(chan error, 1)
	go func() { disableResult <- controller.Disable(context.Background(), "runtime_test") }()
	eventually(t, func() bool {
		state, _ := controller.State("runtime_test")
		return state.Status == RuntimeDisabling
	})
	if next, allowed := controller.Admit("runtime_test"); allowed || next != nil {
		t.Fatal("disabling plugin admitted new call")
	}
	select {
	case <-lifecycle.disableCalled:
		t.Fatal("OnDisable ran before admission drained")
	default:
	}
	admission.Release()
	if err := <-disableResult; err != nil {
		t.Fatal(err)
	}
	select {
	case <-lifecycle.disableCalled:
	default:
		t.Fatal("OnDisable was not called")
	}
	state, _ := controller.State("runtime_test")
	if state.Status != RuntimeDisabled || state.InFlight != 0 {
		t.Fatalf("disabled State() = %+v", state)
	}
	if err := controller.Disable(context.Background(), "runtime_test"); err != nil || lifecycle.disableCalls != 1 {
		t.Fatalf("repeated Disable() = %v calls=%d", err, lifecycle.disableCalls)
	}
}

func TestRuntimeControllerLifecycleFailuresStayFailClosed(t *testing.T) {
	enableFailure := errors.New("enable failed")
	tests := []struct {
		name      string
		lifecycle *runtimeTestLifecycle
		want      error
	}{
		{name: "enable error", lifecycle: &runtimeTestLifecycle{enableErr: enableFailure}, want: enableFailure},
		{name: "enable panic", lifecycle: &runtimeTestLifecycle{panicEnable: true}, want: ErrRuntimeLifecyclePanic},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := newRuntimeTestController(t, test.lifecycle)
			err := controller.Enable(context.Background(), "runtime_test")
			if !errors.Is(err, test.want) {
				t.Fatalf("Enable() error = %v", err)
			}
			state, _ := controller.State("runtime_test")
			if state.Status != RuntimeFailed || !errors.Is(state.LastError, test.want) {
				t.Fatalf("failed State() = %+v", state)
			}
			if admission, admitted := controller.Admit("runtime_test"); admitted || admission != nil {
				t.Fatal("failed plugin admitted")
			}
			if err := controller.Enable(context.Background(), "runtime_test"); !errors.Is(err, ErrRuntimeRecoveryNeeded) {
				t.Fatalf("Enable after failure = %v", err)
			}
			if err := controller.Disable(context.Background(), "runtime_test"); err != nil {
				t.Fatalf("failure cleanup Disable() = %v", err)
			}
		})
	}
}

func TestRuntimeControllerDisableFailuresStayFailClosedAndCanRetry(t *testing.T) {
	disableFailure := errors.New("disable failed")
	tests := []struct {
		name      string
		lifecycle *runtimeTestLifecycle
		want      error
	}{
		{name: "disable error", lifecycle: &runtimeTestLifecycle{disableErr: disableFailure}, want: disableFailure},
		{name: "disable panic", lifecycle: &runtimeTestLifecycle{panicDisable: true}, want: ErrRuntimeLifecyclePanic},
		{name: "disable canceled", lifecycle: &runtimeTestLifecycle{disableErr: context.Canceled}, want: context.Canceled},
		{name: "disable deadline", lifecycle: &runtimeTestLifecycle{disableErr: context.DeadlineExceeded}, want: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := newRuntimeTestController(t, test.lifecycle)
			if err := controller.Enable(context.Background(), "runtime_test"); err != nil {
				t.Fatal(err)
			}
			err := controller.Disable(context.Background(), "runtime_test")
			if !errors.Is(err, test.want) {
				t.Fatalf("Disable() error = %v", err)
			}
			state, _ := controller.State("runtime_test")
			if state.Status != RuntimeFailed || !errors.Is(state.LastError, test.want) {
				t.Fatalf("failed State() = %+v", state)
			}
			if admission, admitted := controller.Admit("runtime_test"); admitted || admission != nil {
				t.Fatal("failed plugin admitted")
			}
			test.lifecycle.mu.Lock()
			test.lifecycle.disableErr = nil
			test.lifecycle.panicDisable = false
			test.lifecycle.mu.Unlock()
			if err := controller.Disable(context.Background(), "runtime_test"); err != nil {
				t.Fatalf("retry Disable() error = %v", err)
			}
		})
	}
}

func TestRuntimeControllerEnableCancellationRequiresCleanup(t *testing.T) {
	lifecycle := &runtimeTestLifecycle{enableErr: context.Canceled}
	controller := newRuntimeTestController(t, lifecycle)
	err := controller.Enable(context.Background(), "runtime_test")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Enable() error = %v", err)
	}
	state, _ := controller.State("runtime_test")
	if state.Status != RuntimeFailed || !errors.Is(state.LastError, context.Canceled) {
		t.Fatalf("cancelled State() = %+v", state)
	}
	if err := controller.Disable(context.Background(), "runtime_test"); err != nil {
		t.Fatalf("cleanup Disable() error = %v", err)
	}
	if lifecycle.disableCalls != 1 {
		t.Fatalf("cleanup disable calls = %d", lifecycle.disableCalls)
	}
}

func TestRuntimeControllerDisableCancellationCanRecover(t *testing.T) {
	controller := newRuntimeTestController(t, nil)
	if err := controller.Enable(context.Background(), "runtime_test"); err != nil {
		t.Fatal(err)
	}
	admission, admitted := controller.Admit("runtime_test")
	if !admitted {
		t.Fatal("ready plugin not admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := controller.Disable(ctx, "runtime_test")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Disable() error = %v", err)
	}
	state, _ := controller.State("runtime_test")
	if state.Status != RuntimeFailed || !errors.Is(state.LastError, context.Canceled) {
		t.Fatalf("cancelled State() = %+v", state)
	}
	admission.Release()
	if err := controller.Disable(context.Background(), "runtime_test"); err != nil {
		t.Fatalf("recovery Disable() error = %v", err)
	}
}

func TestRuntimeControllerRejectsConcurrentTransition(t *testing.T) {
	lifecycle := &runtimeTestLifecycle{enableStarted: make(chan struct{}), enableBlock: make(chan struct{})}
	controller := newRuntimeTestController(t, lifecycle)
	result := make(chan error, 1)
	go func() { result <- controller.Enable(context.Background(), "runtime_test") }()
	<-lifecycle.enableStarted
	if err := controller.Enable(context.Background(), "runtime_test"); !errors.Is(err, ErrRuntimeTransition) {
		t.Fatalf("concurrent Enable() = %v", err)
	}
	if err := controller.Disable(context.Background(), "runtime_test"); !errors.Is(err, ErrRuntimeTransition) {
		t.Fatalf("Disable during enable = %v", err)
	}
	close(lifecycle.enableBlock)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeControllerConcurrentAdmissionsDrainBeforeDisable(t *testing.T) {
	const calls = 64
	controller := newRuntimeTestController(t, nil)
	if err := controller.SetGroupEnabled("runtime_test", 100, true); err != nil {
		t.Fatal(err)
	}
	if err := controller.Enable(context.Background(), "runtime_test"); err != nil {
		t.Fatal(err)
	}
	admissions := make([]Admission, calls)
	for index := range admissions {
		admission, admitted := controller.Admit("runtime_test")
		if !admitted {
			t.Fatalf("Admit(%d) rejected", index)
		}
		admissions[index] = admission
	}
	disableResult := make(chan error, 1)
	go func() { disableResult <- controller.Disable(context.Background(), "runtime_test") }()
	eventually(t, func() bool {
		state, _ := controller.State("runtime_test")
		return state.Status == RuntimeDisabling
	})
	if admission, admitted := controller.Admit("runtime_test"); admitted || admission != nil {
		t.Fatal("new admission won after disabling")
	}
	var waitGroup sync.WaitGroup
	for _, admission := range admissions {
		waitGroup.Add(1)
		go func(admission Admission) {
			defer waitGroup.Done()
			admission.Release()
			admission.Release()
			if admission.GroupEnabled(100) {
				t.Error("released admission retained group access")
			}
		}(admission)
	}
	waitGroup.Wait()
	if err := <-disableResult; err != nil {
		t.Fatal(err)
	}
	state, _ := controller.State("runtime_test")
	if state.Status != RuntimeDisabled || state.InFlight != 0 {
		t.Fatalf("drained State() = %+v", state)
	}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied")
}
