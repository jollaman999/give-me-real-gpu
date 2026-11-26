// Package metrics generates the InfluxDB line protocol that the collection agent
// (telegraf plus the post-processing scripts the monitoring server ships with it)
// writes for an OpenStack GPU cloud: general host/VM metrics, dcgm GPU metrics,
// gpu_vm_mapping, GPM utilization for MIG, XID events and the guest CUDA probe.
//
// The row shapes (measurement, tag keys, field keys and field types) follow what the
// agent pipeline produces after its processors ran, because the monitoring server
// looks rows up by exact tag values.
package metrics

import (
	"math/rand"
	"strconv"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// Options configures a Generator.
type Options struct {
	CredentialID string  // credential id issued by the management platform (credential_id tag, uuid prefix)
	Seed         int64   // random seed for value jitter
	XIDRate      float64 // probability of an XID event per GPU VM per collection
}

// Generator produces the metrics of one credential (one OpenStack cloud).
// It keeps cumulative counters, so one Generator must be used for one series of Collect calls.
type Generator struct {
	opt       Options
	projectID string
	hosts     []*hostState
	vms       []*vmState
	last      time.Time
	slots     map[time.Duration]int64
}

type hostState struct {
	h       *topology.Hypervisor
	r       *rand.Rand
	node    *nodeState
	gpus    []*gpuState
	gpuType string // gpu_type global tag of the hypervisor agent
}

type gpuState struct {
	g    *topology.GPU
	host *hostState
	spec gpuSpec
	r    *rand.Rand
	temp float64
	util float64 // physical utilization of the current collection
	vms  []*vmState
	// fbUsed is the framebuffer in use on the physical GPU (MiB).
	fbUsed float64
}

type vmState struct {
	vm        *topology.VM
	host      *hostState
	gpu       *gpuState
	mig       *topology.MIGInstance
	vgpuIndex int // position of the vGPU on its physical GPU (dcgm gpu tag)
	r         *rand.Rand
	xr        *rand.Rand // separate source so XIDRate does not change any other value
	node      *nodeState
	demand    *walk
	fbFrac    *walk
	dec       *walk
	util      float64
	timeDiff  int // agent clock offset in seconds (time_diff tag)
	license   int
}

// NewGenerator prepares the per-entity state for every hypervisor, GPU and GPU VM of t.
func NewGenerator(t *topology.Topology, opt Options) *Generator {
	if opt.XIDRate < 0 {
		opt.XIDRate = 0
	}
	g := &Generator{
		opt:       opt,
		projectID: t.ProjectID,
		slots:     map[time.Duration]int64{},
	}
	for _, h := range t.Hypervisors {
		hr := newRand(opt.Seed, "host/"+h.Hostname)
		hs := &hostState{
			h:       h,
			r:       hr,
			node:    newNodeState(hr, false, h.VCPUs, h.MemoryMB, h.LocalGB),
			gpuType: hostGPUType(h.Mode),
		}
		for _, gp := range h.GPUs {
			gr := newRand(opt.Seed, "gpu/"+h.Hostname+"/"+gp.UUID)
			sp := specFor(gp.ModelName)
			gs := &gpuState{g: gp, host: hs, spec: sp, r: gr, temp: sp.idleTemp + gr.Float64()*4}
			for i, vm := range gp.VMs {
				key := "vm/" + vm.ID
				vr := newRand(opt.Seed, key)
				vs := &vmState{
					vm:        vm,
					host:      hs,
					gpu:       gs,
					vgpuIndex: i,
					r:         vr,
					xr:        newRand(opt.Seed, "xid/"+vm.ID),
					node:      newNodeState(vr, true, vm.VCPUs, vm.MemoryMB, vm.DiskGB),
					demand:    newWalk(vr, 0, 100, 3, 0.01),
					fbFrac:    newWalk(vr, 0.02, 0.92, 0.01, 0.005),
					dec:       newWalk(vr, 0, 20, 1, 0.01),
					license:   1,
				}
				if hashKey(key)%2 == 1 {
					vs.timeDiff = -1
				}
				if h.Mode == topology.ModeMIG {
					vs.mig = migInstance(gp, vm.MIGGPUInstanceID)
				}
				gs.vms = append(gs.vms, vs)
				g.vms = append(g.vms, vs)
			}
			hs.gpus = append(hs.gpus, gs)
		}
		g.hosts = append(g.hosts, hs)
	}
	return g
}

// Collect returns the points of one collection at now. Each element is one line
// without a trailing newline, timestamps in nanoseconds.
// Inputs that the agent runs on a longer interval (gpu_vm_mapping and the hypervisor
// IP report at 60s, procstat at 60s, the guest CUDA probe at 5m) are emitted only on
// the first call that falls into a new interval slot.
func (g *Generator) Collect(now time.Time) []string {
	dt := 0.0
	if !g.last.IsZero() && now.After(g.last) {
		dt = now.Sub(g.last).Seconds()
	}
	due60 := g.due(time.Minute, now)
	due5m := g.due(5*time.Minute, now)

	for _, hs := range g.hosts {
		for _, gs := range hs.gpus {
			gs.step()
		}
	}

	b := &batch{}
	for _, hs := range g.hosts {
		g.emitHost(b, hs, now, dt, due60)
	}
	for _, vs := range g.vms {
		g.emitVM(b, vs, now, dt, due60, due5m)
	}
	g.last = now
	return b.lines
}

// due reports whether now starts a new slot of the given period and records it.
func (g *Generator) due(period time.Duration, now time.Time) bool {
	slot := now.Unix() / int64(period/time.Second)
	if prev, ok := g.slots[period]; ok && prev == slot {
		return false
	}
	g.slots[period] = slot
	return true
}

func (g *Generator) hostUUID(h *topology.Hypervisor) string {
	return g.opt.CredentialID + ":" + h.Hostname
}

func (g *Generator) vmUUID(vm *topology.VM) string {
	return g.opt.CredentialID + ":" + vm.ID
}

// hostTags are the global tags of a hypervisor agent plus the host and time_diff tags.
func (g *Generator) hostTags(hs *hostState) map[string]string {
	return map[string]string{
		"uuid":            g.hostUUID(hs.h),
		"credential_id":   g.opt.CredentialID,
		"cloud_service":   "openstack",
		"type":            "host",
		"gpu_type":        hs.gpuType,
		"collection_type": "agent",
		"host":            hs.h.Hostname,
		"time_diff":       "0",
	}
}

// vmTags are the global tags of an agent inside a VM. VM agents also get project_id.
func (g *Generator) vmTags(vs *vmState) map[string]string {
	return map[string]string{
		"uuid":            g.vmUUID(vs.vm),
		"credential_id":   g.opt.CredentialID,
		"cloud_service":   "openstack",
		"type":            "vm",
		"gpu_type":        string(vs.host.h.Mode),
		"collection_type": "agent",
		"host":            vs.vm.Name,
		"project_id":      g.projectID,
		"time_diff":       strconv.Itoa(vs.timeDiff),
	}
}

func (g *Generator) emitHost(b *batch, hs *hostState, now time.Time, dt float64, due60 bool) {
	base := g.hostTags(hs)
	ts := now.UnixNano()
	st := now.Unix()

	hs.node.emit(b, base, ts, st, dt, 0, due60)

	// The hypervisor IP report (exec, 60s): "net ip=..." with only the global tags.
	// The management platform reads it with SELECT LAST(ip) FROM net GROUP BY uuid.
	if due60 && hs.h.HostIP != "" {
		b.add("net", base, map[string]any{"ip": hs.h.HostIP, "server_time": st}, ts)
	}

	// Physical GPUs are read by nvidia-smi on VGPU and MIG hypervisors only.
	// A passthrough hypervisor has its GPUs bound to vfio-pci, so it reports none.
	if hs.h.Mode != topology.ModePassthrough {
		for _, gs := range hs.gpus {
			g.emitPhysical(b, hs, gs, ts, st)
		}
	}

	if due60 {
		g.emitMapping(b, hs, ts, st)
	}
}

func (g *Generator) emitVM(b *batch, vs *vmState, now time.Time, dt float64, due60, due5m bool) {
	base := g.vmTags(vs)
	shifted := now.Add(time.Duration(vs.timeDiff) * time.Second)
	ts := shifted.UnixNano()
	st := now.Unix()

	vs.node.emit(b, base, ts, st, dt, vs.util, due60)

	switch vs.host.h.Mode {
	case topology.ModePassthrough:
		g.emitPassthroughGuest(b, vs, base, ts)
	default:
		if due5m {
			g.emitCUDAProbe(b, vs, base, ts, st)
		}
	}

	if g.opt.XIDRate > 0 && vs.xr.Float64() < g.opt.XIDRate {
		g.emitXID(b, vs, now)
	}
}

// hostGPUType is the gpu_type global tag the monitoring server writes into a hypervisor
// agent config. It is detected from `nvidia-smi -q -x` on the node:
//   - a vGPU host driver reports virtualization_mode "Host VGPU", which is neither
//     "VGPU" nor "Pass-Through", so the detector falls back to BARE_METAL;
//   - MIG mode enabled gives MIG;
//   - a passthrough host has no nvidia driver, the output is empty, which gives NONE.
func hostGPUType(m topology.GPUMode) string {
	switch m {
	case topology.ModeVGPU:
		return "BARE_METAL"
	case topology.ModeMIG:
		return "MIG"
	default:
		return "NONE"
	}
}

func migInstance(gp *topology.GPU, id int) *topology.MIGInstance {
	for _, mi := range gp.MIGInstances {
		if mi.GPUInstanceID == id {
			return mi
		}
	}
	return nil
}
