package runtime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

type fakeJob struct {
	lines    int
	writeErr error
	block    bool // Write waits until ctx is cancelled

	collects atomic.Int32
	writes   atomic.Int32
	mu       sync.Mutex
	lastCtx  context.Context
}

func (j *fakeJob) Collect(now time.Time) (any, int) {
	j.collects.Add(1)
	return "payload", j.lines
}

func (j *fakeJob) Write(ctx context.Context, payload any) error {
	j.mu.Lock()
	j.lastCtx = ctx
	j.mu.Unlock()
	j.writes.Add(1)
	if j.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return j.writeErr
}

func (j *fakeJob) ctxDone() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lastCtx != nil && j.lastCtx.Err() != nil
}

// builder records every Build call and hands out the job made by mk.
type builder struct {
	mu   sync.Mutex
	jobs map[uint][]*fakeJob
	errs map[uint]error
	mk   func(SinkSpec) *fakeJob
}

func newBuilder(mk func(SinkSpec) *fakeJob) *builder {
	if mk == nil {
		mk = func(SinkSpec) *fakeJob { return &fakeJob{lines: 3} }
	}
	return &builder{jobs: map[uint][]*fakeJob{}, errs: map[uint]error{}, mk: mk}
}

func (b *builder) Build(sp SinkSpec) (Job, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.errs[sp.ID]; err != nil {
		return nil, err
	}
	j := b.mk(sp)
	b.jobs[sp.ID] = append(b.jobs[sp.ID], j)
	return j, nil
}

func (b *builder) builds(id uint) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.jobs[id])
}

func (b *builder) last(id uint) *fakeJob {
	b.mu.Lock()
	defer b.mu.Unlock()
	js := b.jobs[id]
	return js[len(js)-1]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func statusByID(s *Supervisor) map[uint]SinkStatus {
	out := map[uint]SinkStatus{}
	for _, st := range s.Status() {
		out[st.ID] = st
	}
	return out
}

func spec(id uint, key string, enabled bool) SinkSpec {
	return SinkSpec{ID: id, Name: "site-" + string(rune('a'+id-1)), Type: "influxdb", Enabled: enabled, Key: key}
}

func TestApplySameKeyKeepsLoop(t *testing.T) {
	b := newBuilder(nil)
	s := NewSupervisor()
	defer s.Stop()
	cfg := Config{Interval: time.Hour, Sinks: []SinkSpec{spec(1, "a", true), spec(2, "b", true)}, Build: b.Build}
	s.Apply(cfg)
	waitFor(t, "first writes", func() bool { return b.last(1).writes.Load() == 1 && b.last(2).writes.Load() == 1 })

	for range 3 {
		s.Apply(cfg)
	}
	for _, id := range []uint{1, 2} {
		if got := b.builds(id); got != 1 {
			t.Errorf("sink %d built %d times, want 1", id, got)
		}
		if got := b.last(id).collects.Load(); got != 1 {
			t.Errorf("sink %d collected %d times, want 1", id, got)
		}
		if b.last(id).ctxDone() {
			t.Errorf("sink %d loop was cancelled", id)
		}
	}
}

func TestApplyRestartsOnlyChangedSinks(t *testing.T) {
	b := newBuilder(func(sp SinkSpec) *fakeJob { return &fakeJob{lines: 3, block: sp.ID == 1} })
	s := NewSupervisor()
	defer s.Stop()
	s.Apply(Config{Interval: time.Hour, Build: b.Build, Sinks: []SinkSpec{
		spec(1, "a", true), spec(2, "b", true), spec(3, "c", true), spec(4, "d", true),
	}})
	waitFor(t, "first writes", func() bool {
		for id := uint(1); id <= 4; id++ {
			if b.last(id).writes.Load() != 1 {
				return false
			}
		}
		return true
	})
	old1, old3, old4 := b.last(1), b.last(3), b.last(4)

	// 1: key changed while its write is in progress, 2: unchanged, 3: disabled, 4: removed.
	s.Apply(Config{Interval: time.Hour, Build: b.Build, Sinks: []SinkSpec{
		spec(1, "a2", true), spec(2, "b", true), spec(3, "c", false),
	}})

	for id, want := range map[uint]int{1: 2, 2: 1, 3: 1, 4: 1} {
		if got := b.builds(id); got != want {
			t.Errorf("sink %d built %d times, want %d", id, got, want)
		}
	}
	for name, j := range map[string]*fakeJob{"old 1": old1, "3": old3, "4": old4} {
		if !j.ctxDone() {
			t.Errorf("sink %s loop context is not cancelled", name)
		}
	}
	if b.last(2).ctxDone() {
		t.Error("unchanged sink 2 loop was cancelled")
	}
	waitFor(t, "restarted sink 1 write", func() bool { return b.last(1) != old1 && b.last(1).writes.Load() == 1 })

	st := s.Status()
	if len(st) != 3 {
		t.Fatalf("status has %d sinks, want 3: %+v", len(st), st)
	}
	if st[0].ID != 1 || st[1].ID != 2 || st[2].ID != 3 {
		t.Errorf("status order = %d,%d,%d, want 1,2,3", st[0].ID, st[1].ID, st[2].ID)
	}
	if st[2].Enabled {
		t.Error("sink 3 status is enabled, want disabled")
	}

	// A name change restarts the loop as well, because the loop logs with the name.
	renamed := spec(2, "b", true)
	renamed.Name = "site-z"
	s.Apply(Config{Interval: time.Hour, Build: b.Build, Sinks: []SinkSpec{spec(1, "a2", true), renamed}})
	if got := b.builds(2); got != 2 {
		t.Errorf("renamed sink 2 built %d times, want 2", got)
	}
	if got := s.Status()[1].Name; got != "site-z" {
		t.Errorf("sink 2 status name = %q, want site-z", got)
	}
}

func TestIntervalOrDryRunChangeRestartsAll(t *testing.T) {
	b := newBuilder(nil)
	s := NewSupervisor()
	defer s.Stop()
	sinks := []SinkSpec{spec(1, "a", true), spec(2, "b", true)}
	s.Apply(Config{Interval: time.Hour, Sinks: sinks, Build: b.Build})
	s.Apply(Config{Interval: 2 * time.Hour, Sinks: sinks, Build: b.Build})
	for _, id := range []uint{1, 2} {
		if got := b.builds(id); got != 2 {
			t.Errorf("after interval change sink %d built %d times, want 2", id, got)
		}
	}
	s.Apply(Config{Interval: 2 * time.Hour, DryRun: true, Sinks: sinks, Build: b.Build})
	for _, id := range []uint{1, 2} {
		if got := b.builds(id); got != 3 {
			t.Errorf("after dry_run change sink %d built %d times, want 3", id, got)
		}
	}
}

func TestDryRunCollectsWithoutWriting(t *testing.T) {
	b := newBuilder(func(SinkSpec) *fakeJob { return &fakeJob{lines: 7} })
	s := NewSupervisor()
	defer s.Stop()
	s.Apply(Config{Interval: time.Hour, DryRun: true, Sinks: []SinkSpec{spec(1, "a", true)}, Build: b.Build})
	waitFor(t, "dry run lines in status", func() bool { return statusByID(s)[1].Lines == 7 })
	if got := b.last(1).writes.Load(); got != 0 {
		t.Errorf("dry run wrote %d times, want 0", got)
	}
	if st := statusByID(s)[1]; !st.LastWrite.IsZero() || st.LastError != "" {
		t.Errorf("dry run status = %+v, want no last write and no error", st)
	}
}

func TestBuildErrorKeepsOtherSinks(t *testing.T) {
	b := newBuilder(nil)
	b.errs[1] = errors.New("bad url")
	s := NewSupervisor()
	defer s.Stop()
	s.Apply(Config{Interval: time.Hour, Sinks: []SinkSpec{spec(1, "a", true), spec(2, "b", true)}, Build: b.Build})

	if got := statusByID(s)[1].LastError; got != "bad url" {
		t.Errorf("sink 1 LastError = %q, want %q", got, "bad url")
	}
	waitFor(t, "sink 2 write", func() bool { return b.last(2).writes.Load() == 1 })

	b.mu.Lock()
	delete(b.errs, 1)
	b.mu.Unlock()
	s.Apply(Config{Interval: time.Hour, Sinks: []SinkSpec{spec(1, "a-fixed", true), spec(2, "b", true)}, Build: b.Build})
	if got := statusByID(s)[1].LastError; got != "" {
		t.Errorf("sink 1 LastError after fix = %q, want empty", got)
	}
	waitFor(t, "sink 1 write", func() bool { return b.builds(1) == 1 && b.last(1).writes.Load() == 1 })
	if got := b.builds(2); got != 1 {
		t.Errorf("sink 2 built %d times, want 1", got)
	}
}

func TestInvalidIntervalStartsNothing(t *testing.T) {
	b := newBuilder(nil)
	s := NewSupervisor()
	defer s.Stop()
	s.Apply(Config{Interval: 0, Sinks: []SinkSpec{spec(1, "a", true)}, Build: b.Build})
	if got := b.builds(1); got != 0 {
		t.Errorf("built %d times with zero interval, want 0", got)
	}
	if got := statusByID(s)[1].LastError; got != "interval must be positive" {
		t.Errorf("LastError = %q", got)
	}
}

func TestWriteStatus(t *testing.T) {
	b := newBuilder(func(sp SinkSpec) *fakeJob {
		if sp.ID == 2 {
			return &fakeJob{lines: 5, writeErr: errors.New("HTTP 500")}
		}
		return &fakeJob{lines: 4}
	})
	s := NewSupervisor()
	defer s.Stop()
	before := time.Now()
	s.Apply(Config{Interval: time.Hour, Sinks: []SinkSpec{spec(1, "a", true), spec(2, "b", true)}, Build: b.Build})
	waitFor(t, "status after writes", func() bool {
		st := statusByID(s)
		return !st[1].LastWrite.IsZero() && st[2].LastError != ""
	})
	st := statusByID(s)
	if ok := st[1]; ok.Lines != 4 || ok.LastError != "" || ok.LastWrite.Before(before) || ok.Name != "site-a" || ok.Type != "influxdb" || !ok.Enabled {
		t.Errorf("successful sink status = %+v", ok)
	}
	if bad := st[2]; bad.LastError != "HTTP 500" || !bad.LastWrite.IsZero() || bad.Lines != 0 {
		t.Errorf("failing sink status = %+v", bad)
	}
}

func TestLoopRepeatsEveryInterval(t *testing.T) {
	b := newBuilder(nil)
	s := NewSupervisor()
	defer s.Stop()
	s.Apply(Config{Interval: 10 * time.Millisecond, Sinks: []SinkSpec{spec(1, "a", true)}, Build: b.Build})
	waitFor(t, "three writes", func() bool { return b.last(1).writes.Load() >= 3 })
}

func TestStopLeavesNoGoroutines(t *testing.T) {
	base := goruntime.NumGoroutine()
	b := newBuilder(func(sp SinkSpec) *fakeJob { return &fakeJob{lines: 1, block: sp.ID == 1} })
	s := NewSupervisor()
	s.Apply(Config{Interval: 5 * time.Millisecond, Build: b.Build, Sinks: []SinkSpec{
		spec(1, "a", true), spec(2, "b", true), spec(3, "c", true),
	}})
	waitFor(t, "writes", func() bool { return b.last(1).writes.Load() == 1 && b.last(3).writes.Load() >= 2 })
	s.Stop()

	for id := uint(1); id <= 3; id++ {
		if !b.last(id).ctxDone() {
			t.Errorf("sink %d context is not cancelled after Stop", id)
		}
	}
	waitFor(t, "goroutines to exit", func() bool { return goruntime.NumGoroutine() <= base })

	s.Apply(Config{Interval: time.Hour, Build: b.Build, Sinks: []SinkSpec{spec(4, "d", true)}})
	if got := b.builds(4); got != 0 {
		t.Errorf("Apply after Stop built %d jobs, want 0", got)
	}
}
