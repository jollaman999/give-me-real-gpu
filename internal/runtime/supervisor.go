// Package runtime applies saved settings to the running process without a restart.
// Supervisor keeps one write loop per enabled sink and restarts only the loops whose configuration changed.
// OpenStackSwitch swaps the fake OpenStack handler behind a fixed listener.
package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// SinkSpec describes one sink as far as the Supervisor needs to know it.
//
// Key is an opaque serialization of everything the sink's Job depends on (URL, credentials, and so on).
// It may contain secrets and is never logged. A running loop is kept as long as its Key, Name and Type stay the same.
// The caller must fold anything else the Job is built from into Key as well, such as the topology and the seed,
// so that changing those restarts the loop.
type SinkSpec struct {
	ID      uint
	Name    string
	Type    string
	Enabled bool
	Key     string
}

// Job generates and writes metrics for one sink.
// Write must return soon after ctx is cancelled; the Supervisor waits for it when it stops a loop.
type Job interface {
	Collect(now time.Time) (payload any, lines int)
	Write(ctx context.Context, payload any) error
}

// Config is the full desired state passed to Apply.
type Config struct {
	Interval time.Duration
	DryRun   bool
	Sinks    []SinkSpec
	Build    func(SinkSpec) (Job, error)
}

// SinkStatus is the state of one sink as of the last Apply and the last write.
// LastWrite and Elapsed are from the last successful write. LastError is cleared by a successful write.
type SinkStatus struct {
	ID        uint
	Name      string
	Type      string
	Enabled   bool
	LastWrite time.Time
	Lines     int
	Elapsed   time.Duration
	LastError string
}

// Supervisor runs the write loops. Create it with NewSupervisor.
type Supervisor struct {
	// applyMu serializes Apply and Stop and guards the fields below it.
	applyMu  sync.Mutex
	applied  bool
	stopped  bool
	interval time.Duration
	dryRun   bool
	loops    map[uint]*loop

	// mu guards order and states. It is never held while waiting for a loop.
	mu     sync.Mutex
	order  []uint
	states map[uint]*sinkState
}

type loop struct {
	spec   SinkSpec
	cancel context.CancelFunc
	done   chan struct{}
}

type sinkState struct {
	mu sync.Mutex
	st SinkStatus
}

// NewSupervisor returns a Supervisor with no loops running.
func NewSupervisor() *Supervisor {
	return &Supervisor{
		loops:  map[uint]*loop{},
		states: map[uint]*sinkState{},
	}
}

// Apply makes the running loops match cfg.
// Loops of removed or disabled sinks and of sinks whose Key, Name or Type changed are stopped, and changed sinks are started again.
// A change of Interval or DryRun restarts every loop. A Build error is kept as that sink's LastError and the other sinks go on.
// Stopping a loop cancels the context of a write in progress and waits for the loop to return.
// Apply after Stop does nothing.
func (s *Supervisor) Apply(cfg Config) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	if s.stopped {
		return
	}

	restartAll := s.applied && (cfg.Interval != s.interval || cfg.DryRun != s.dryRun)
	s.applied, s.interval, s.dryRun = true, cfg.Interval, cfg.DryRun

	want := make(map[uint]SinkSpec, len(cfg.Sinks))
	order := make([]uint, 0, len(cfg.Sinks))
	for _, sp := range cfg.Sinks {
		if _, dup := want[sp.ID]; dup {
			slog.Warn("duplicate sink id; ignoring the later one", "id", sp.ID, "sink", sp.Name)
			continue
		}
		want[sp.ID] = sp
		order = append(order, sp.ID)
	}

	var stopping []*loop
	for id, l := range s.loops {
		sp, ok := want[id]
		if restartAll || !ok || !sp.Enabled || !sameLoop(l.spec, sp) {
			l.cancel()
			stopping = append(stopping, l)
			delete(s.loops, id)
		}
	}
	for _, l := range stopping {
		<-l.done
	}

	s.mu.Lock()
	states := make(map[uint]*sinkState, len(order))
	for _, id := range order {
		st := s.states[id]
		if st == nil {
			st = &sinkState{}
		}
		sp := want[id]
		st.mu.Lock()
		st.st.ID, st.st.Name, st.st.Type, st.st.Enabled = sp.ID, sp.Name, sp.Type, sp.Enabled
		st.mu.Unlock()
		states[id] = st
	}
	s.order, s.states = order, states
	s.mu.Unlock()

	for _, id := range order {
		if _, running := s.loops[id]; running {
			continue
		}
		sp, st := want[id], states[id]
		if !sp.Enabled {
			st.setError("")
			continue
		}
		job, err := build(cfg, sp)
		if err != nil {
			slog.Error("sink start failed", "sink", sp.Name, "type", sp.Type, "err", err)
			st.setError(err.Error())
			continue
		}
		st.setError("")
		ctx, cancel := context.WithCancel(context.Background())
		l := &loop{spec: sp, cancel: cancel, done: make(chan struct{})}
		s.loops[id] = l
		go func() {
			defer close(l.done)
			writeLoop(ctx, cfg.Interval, cfg.DryRun, sp, job, st)
		}()
	}
}

func build(cfg Config, sp SinkSpec) (Job, error) {
	if cfg.Interval <= 0 {
		return nil, errors.New("interval must be positive")
	}
	if cfg.Build == nil {
		return nil, errors.New("no job builder")
	}
	job, err := cfg.Build(sp)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, errors.New("job builder returned no job")
	}
	return job, nil
}

func sameLoop(a, b SinkSpec) bool {
	return a.Key == b.Key && a.Name == b.Name && a.Type == b.Type
}

// Status returns the sinks in the order of the last Apply.
func (s *Supervisor) Status() []SinkStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SinkStatus, 0, len(s.order))
	for _, id := range s.order {
		st := s.states[id]
		st.mu.Lock()
		out = append(out, st.st)
		st.mu.Unlock()
	}
	return out
}

// Stop stops every loop and waits for them to return. Later calls to Apply do nothing.
func (s *Supervisor) Stop() {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.stopped = true
	for _, l := range s.loops {
		l.cancel()
	}
	for id, l := range s.loops {
		<-l.done
		delete(s.loops, id)
	}
}

func (st *sinkState) setError(msg string) {
	st.mu.Lock()
	st.st.LastError = msg
	st.mu.Unlock()
}

// writeLoop collects and writes once immediately and then every interval until ctx is cancelled.
func writeLoop(ctx context.Context, interval time.Duration, dryRun bool, sp SinkSpec, job Job, st *sinkState) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	now := time.Now()
	for {
		payload, lines := job.Collect(now)
		if dryRun {
			slog.Info("dry_run: not writing", "sink", sp.Name, "type", sp.Type, "lines", lines)
			st.mu.Lock()
			st.st.Lines = lines
			st.mu.Unlock()
		} else {
			start := time.Now()
			err := job.Write(ctx, payload)
			if err != nil && ctx.Err() != nil {
				return
			}
			elapsed := time.Since(start)
			st.mu.Lock()
			if err != nil {
				st.st.LastError = err.Error()
			} else {
				st.st.LastWrite, st.st.Lines, st.st.Elapsed, st.st.LastError = time.Now(), lines, elapsed, ""
			}
			st.mu.Unlock()
			if err != nil {
				slog.Error("sink write failed", "sink", sp.Name, "type", sp.Type, "lines", lines, "err", err)
			} else {
				slog.Info("sink write", "sink", sp.Name, "type", sp.Type, "lines", lines, "elapsed", elapsed.Round(time.Millisecond))
			}
		}
		select {
		case <-ctx.Done():
			return
		case now = <-ticker.C:
		}
	}
}
