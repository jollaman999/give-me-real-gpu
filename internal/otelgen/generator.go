// Package otelgen generates the metrics an OpenTelemetry-based collection agent
// sends over OTLP from every node of the topology: the hostmetrics receiver
// (system.*, process.*) and an NVIDIA GPU receiver reading nvidia-smi
// (nvidia_smi.<query field>), the kernel log (nvidia_kmsg.xid.errors) and
// NVML GPM (nvml.gpm.*).
//
// Every hypervisor and every VM is one agent. The agent stamps entity_id, the
// node's key in the management platform, on every data point, which is the
// only label the monitoring server selects a node by. Names, units, metric
// types and attributes follow the receivers, so a backend that stores OTLP
// under Prometheus names (dots to underscores, unit suffix, _total on
// counters) gets e.g. system_cpu_time_seconds_total and
// nvidia_smi_utilization_gpu_percent.
package otelgen

import (
	"strconv"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/otlp"
	"github.com/jollaman999/give-me-real-gpu/internal/tagrules"
	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// LabelEntityID is the attribute every data point carries.
const LabelEntityID = "entity_id"

// Options configures a Generator.
type Options struct {
	Seed     int64           // random seed for value jitter
	XIDRate  float64         // probability of an XID event per GPU VM per collection
	TagRules []tagrules.Rule // applied in order to the data point and resource attributes. Metric names stay as they are
}

// Generator produces the metrics of every node of one topology.
// It keeps cumulative counters, so one Generator must be used for one series of Collect calls.
type Generator struct {
	opt     Options
	agents  []*agent
	gpus    []*physGPU
	vgpus   []*vmGPU
	started bool
	last    time.Time
}

// agent is one collection agent: one hypervisor or one VM.
type agent struct {
	name     string
	entityID string
	sys      *sysState
	gpus     []*gpuView
	xid      map[xidKey]int64
	xidOrder []xidKey
	boot     time.Time
	uptime0  time.Duration // uptime at the first collection
}

type xidKey struct {
	uuid string
	xid  int64
}

// NewGenerator prepares the per-node state of every hypervisor, GPU and VM of t.
func NewGenerator(t *topology.Topology, opt Options) *Generator {
	if opt.XIDRate < 0 {
		opt.XIDRate = 0
	}
	g := &Generator{opt: opt}
	vmAgents := map[*topology.VM]*agent{}
	var hostAgents []*agent
	for _, h := range t.Hypervisors {
		hr := newRand(opt.Seed, "otel/host/"+h.Hostname)
		ha := &agent{
			name:     h.Hostname,
			entityID: topology.EntityID(h.Hostname, h.EntityID),
			xid:      map[xidKey]int64{},
			uptime0:  time.Duration(30+hr.Intn(210)) * 24 * time.Hour,
		}
		ha.sys = newHostSystem(hr, h, ha.uptime0)
		hostAgents = append(hostAgents, ha)

		hostIndex := 0
		for _, gp := range h.GPUs {
			pg := newPhysGPU(opt.Seed, h, gp)
			g.gpus = append(g.gpus, pg)
			if hostSees(h, gp) {
				ha.gpus = append(ha.gpus, &gpuView{kind: viewHost, index: hostIndex, uuid: gp.UUID, name: gp.ModelName, phys: pg})
				pg.hostAgent = ha
				hostIndex++
			}
			for _, vm := range gp.VMs {
				vr := newRand(opt.Seed, "otel/vm/"+vm.ID)
				va := &agent{
					name:     vm.Name,
					entityID: topology.EntityID(vm.Name, vm.EntityID),
					xid:      map[xidKey]int64{},
					uptime0:  time.Duration(1+vr.Intn(90))*24*time.Hour + time.Duration(vr.Intn(86400))*time.Second,
				}
				va.sys = newVMSystem(vr, vm, va.uptime0)
				vg := newVMGPU(opt.Seed, vr, h, pg, vm)
				pg.vms = append(pg.vms, vg)
				g.vgpus = append(g.vgpus, vg)
				vg.agent = va
				if v := guestView(h, pg, vg); v != nil {
					va.gpus = append(va.gpus, v)
				}
				vmAgents[vm] = va
			}
		}
		ha.sys.addHostProcesses(hr, h, vmsOf(g.gpus, h))
	}
	g.agents = append(g.agents, hostAgents...)
	for _, vm := range t.AllVMs() {
		if a := vmAgents[vm]; a != nil {
			g.agents = append(g.agents, a)
		}
	}
	return g
}

// vmsOf lists the VMs of hypervisor h in topology order.
func vmsOf(gpus []*physGPU, h *topology.Hypervisor) []*vmGPU {
	var out []*vmGPU
	for _, pg := range gpus {
		if pg.host == h {
			out = append(out, pg.vms...)
		}
	}
	return out
}

// Collect produces one collection of every agent at now.
func (g *Generator) Collect(now time.Time) *otlp.ExportRequest {
	dt := 0.0
	if !g.started {
		g.started = true
		for _, a := range g.agents {
			a.boot = now.Add(-a.uptime0)
		}
	} else if d := now.Sub(g.last).Seconds(); d > 0 {
		dt = d
	}
	g.last = now

	for _, pg := range g.gpus {
		pg.step()
	}
	g.xidEvents()

	req := &otlp.ExportRequest{}
	for _, a := range g.agents {
		req.ResourceMetrics = append(req.ResourceMetrics, a.collect(now, dt)...)
	}
	tagrules.ApplyRequest(g.opt.TagRules, req)
	return req
}

// LabelKeys returns the attribute keys one first collection of t emits, before any label rule, with an example
// value and whether each key is on a "resource", a data "point" or both. XID events are forced on so their
// attributes are listed too. Integer attributes are listed with their decimal value.
func LabelKeys(t *topology.Topology, now time.Time) []tagrules.KeyInfo {
	req := NewGenerator(t, Options{Seed: 1, XIDRate: 1}).Collect(now)
	keys := &tagrules.KeySet{}
	add := func(where string, kv otlp.KeyValue) {
		v := kv.Str
		if kv.IsInt {
			v = strconv.FormatInt(kv.Int, 10)
		}
		keys.Add(where, kv.Key, v)
	}
	for _, rm := range req.ResourceMetrics {
		for _, kv := range rm.Resource {
			add("resource", kv)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				for _, p := range m.Points {
					for _, kv := range p.Attributes {
						add("point", kv)
					}
				}
			}
		}
	}
	return keys.List()
}

// collect builds the resources of one agent: the host resource (hostmetrics
// scrapers and the GPU receiver) followed by one resource per process kept.
func (a *agent) collect(now time.Time, dt float64) []*otlp.ResourceMetrics {
	e := &emitter{
		entity: otlp.String(LabelEntityID, a.entityID),
		ts:     uint64(now.UnixNano()),
		start:  uint64(a.boot.UnixNano()),
	}
	host := &otlp.ResourceMetrics{}
	a.sys.emit(host, e, now.Sub(a.boot), dt)
	a.emitGPU(host, e)
	out := []*otlp.ResourceMetrics{host}
	out = append(out, a.sys.emitProcesses(e, a.boot, dt)...)
	return out
}

// emitter adds data points that all carry the agent's entity_id.
type emitter struct {
	entity otlp.KeyValue
	ts     uint64
	start  uint64 // start time of cumulative sums (boot time)
}

// scope collects the metrics of one instrumentation scope, one Metric per name.
type scope struct {
	sm     *otlp.ScopeMetrics
	byName map[string]*otlp.Metric
}

func newScope(rm *otlp.ResourceMetrics, name string) *scope {
	sm := &otlp.ScopeMetrics{ScopeName: name}
	rm.ScopeMetrics = append(rm.ScopeMetrics, sm)
	return &scope{sm: sm, byName: map[string]*otlp.Metric{}}
}

func (s *scope) metric(name, unit string, typ otlp.MetricType, monotonic bool) *otlp.Metric {
	if m, ok := s.byName[name]; ok {
		return m
	}
	m := &otlp.Metric{Name: name, Unit: unit, Type: typ, Monotonic: monotonic}
	s.sm.Metrics = append(s.sm.Metrics, m)
	s.byName[name] = m
	return m
}

func (s *scope) gauge(name, unit string) *otlp.Metric {
	return s.metric(name, unit, otlp.Gauge, false)
}

// sum is a cumulative sum; monotonic for counters, not for up-down counters (usage in bytes).
func (s *scope) sum(name, unit string, monotonic bool) *otlp.Metric {
	return s.metric(name, unit, otlp.Sum, monotonic)
}

func (e *emitter) point(m *otlp.Metric, start uint64, attrs []otlp.KeyValue) otlp.NumberDataPoint {
	p := otlp.NumberDataPoint{TimeUnixNano: e.ts, Attributes: append(attrs, e.entity)}
	if m.Type == otlp.Sum {
		p.StartTimeUnixNano = start
	}
	return p
}

// double adds a double point.
func (e *emitter) double(m *otlp.Metric, v float64, attrs ...otlp.KeyValue) {
	p := e.point(m, e.start, attrs)
	p.Double = v
	m.Points = append(m.Points, p)
}

// int adds an integer point.
func (e *emitter) int(m *otlp.Metric, v int64, attrs ...otlp.KeyValue) {
	p := e.point(m, e.start, attrs)
	p.Int, p.IsInt = v, true
	m.Points = append(m.Points, p)
}

// withStart returns an emitter whose cumulative sums start at start (a process start time).
func (e *emitter) withStart(start time.Time) *emitter {
	c := *e
	c.start = uint64(start.UnixNano())
	return &c
}
