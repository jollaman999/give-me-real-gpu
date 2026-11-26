package otelgen

import (
	"math/rand"
	"slices"
	"strconv"
	"strings"

	"github.com/jollaman999/give-me-real-gpu/internal/otlp"
	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// gpuScope is the instrumentation scope of the GPU receiver's metrics.
const gpuScope = "github.com/jollaman999/give-me-real-gpu/internal/otelgen/nvidiagpu"

// migSlices is the number of compute slices of a MIG capable GPU (A100, H100).
const migSlices = 7

// gpuSpec holds the model dependent ranges used to make values plausible.
type gpuSpec struct {
	tdp      float64 // board power limit (W)
	idleTemp float64 // idle GPU temperature (C)
	smBoost  float64 // SM clock under load (MHz)
	smIdle   float64 // SM clock at P8 (MHz)
	memBusy  float64 // memory clock under load (MHz)
	memIdle  float64 // memory clock at P8 (MHz)
	hbm      bool    // HBM boards report temperature.memory
	fan      bool    // actively cooled boards report fan.speed; passive data center boards report [N/A]
	gpm      bool    // Hopper and later have NVML GPM
}

func specFor(model, arch string) gpuSpec {
	m := strings.ToUpper(model)
	var s gpuSpec
	switch {
	case strings.Contains(m, "T4"):
		s = gpuSpec{tdp: 70, idleTemp: 34, smBoost: 1590, smIdle: 300, memBusy: 5000, memIdle: 405}
	case strings.Contains(m, "A100") && strings.Contains(m, "PCIE"):
		s = gpuSpec{tdp: 250, idleTemp: 33, smBoost: 1410, smIdle: 1410, memBusy: 1215, memIdle: 1215, hbm: true}
	case strings.Contains(m, "A100"):
		s = gpuSpec{tdp: 400, idleTemp: 32, smBoost: 1410, smIdle: 1410, memBusy: 1593, memIdle: 1593, hbm: true}
	case strings.Contains(m, "H100"), strings.Contains(m, "H200"):
		s = gpuSpec{tdp: 700, idleTemp: 31, smBoost: 1980, smIdle: 1980, memBusy: 2619, memIdle: 2619, hbm: true}
	case strings.Contains(m, "A10"):
		s = gpuSpec{tdp: 150, idleTemp: 36, smBoost: 1695, smIdle: 210, memBusy: 6251, memIdle: 405}
	case strings.Contains(m, "L40"):
		s = gpuSpec{tdp: 350, idleTemp: 35, smBoost: 2520, smIdle: 210, memBusy: 9001, memIdle: 405}
	default:
		s = gpuSpec{tdp: 300, idleTemp: 34, smBoost: 1500, smIdle: 300, memBusy: 5000, memIdle: 405}
	}
	s.fan = strings.Contains(m, "RTX") || strings.Contains(m, "GEFORCE") || strings.Contains(m, "QUADRO")
	switch arch {
	case "Hopper", "Blackwell":
		s.gpm = true
	}
	return s
}

// physGPU is one physical GPU and the values of the current collection.
type physGPU struct {
	g         *topology.GPU
	host      *topology.Hypervisor
	spec      gpuSpec
	r         *rand.Rand
	gpmR      *rand.Rand
	vms       []*vmGPU
	hostAgent *agent // nil when the hypervisor cannot see this GPU with nvidia-smi
	eccAgg    float64

	temp, util, fbUsed       float64
	power, smClock, memClock float64
	reasons, decUtil         float64
}

func newPhysGPU(seed int64, h *topology.Hypervisor, gp *topology.GPU) *physGPU {
	r := newRand(seed, "otel/gpu/"+gp.UUID)
	sp := specFor(gp.ModelName, gp.Arch)
	return &physGPU{
		g: gp, host: h, spec: sp, r: r,
		gpmR:   newRand(seed, "otel/gpm/"+gp.UUID),
		temp:   sp.idleTemp + r.Float64()*4,
		eccAgg: float64(hashKey(gp.UUID) % 3),
	}
}

// vmGPU is the GPU share of one VM.
type vmGPU struct {
	vm     *topology.VM
	phys   *physGPU
	mig    *topology.MIGInstance
	r      *rand.Rand
	xr     *rand.Rand // separate source so XIDRate does not change any other value
	demand *walk
	fbFrac *walk
	dec    *walk
	agent  *agent
	util   float64
	fbUsed float64
}

func newVMGPU(seed int64, r *rand.Rand, h *topology.Hypervisor, pg *physGPU, vm *topology.VM) *vmGPU {
	vg := &vmGPU{
		vm: vm, phys: pg, r: r,
		xr:     newRand(seed, "otel/xid/"+vm.ID),
		demand: newWalk(r, 0, 100, 3, 0.01),
		fbFrac: newWalk(r, 0.02, 0.92, 0.01, 0.005),
		dec:    newWalk(r, 0, 20, 1, 0.01),
	}
	if h.Mode == topology.ModeMIG {
		for _, mi := range pg.g.MIGInstances {
			if mi.GPUInstanceID == vm.MIGGPUInstanceID {
				vg.mig = mi
			}
		}
	}
	return vg
}

// hostSees reports whether nvidia-smi on the hypervisor lists gp. It needs the
// host driver; a passthrough GPU given to a VM is bound to vfio-pci and the
// host driver does not see it.
func hostSees(h *topology.Hypervisor, gp *topology.GPU) bool {
	if h.DriverVersion == "" {
		return false
	}
	return !(h.Mode == topology.ModePassthrough && len(gp.VMs) > 0)
}

// viewKind is who looks at a GPU with nvidia-smi.
type viewKind int

const (
	viewHost        viewKind = iota // the hypervisor, physical GPU
	viewVGPU                        // a VM with a time-sliced vGPU
	viewMIGGuest                    // a VM with a MIG-backed vGPU
	viewPassthrough                 // a VM owning the whole physical GPU
)

// gpuView is a GPU as one agent's nvidia-smi lists it.
type gpuView struct {
	kind  viewKind
	index int
	uuid  string
	name  string
	phys  *physGPU
	vg    *vmGPU
}

// guestView is the GPU the VM sees, nil without a guest driver.
func guestView(h *topology.Hypervisor, pg *physGPU, vg *vmGPU) *gpuView {
	if vg.vm.GuestDriverVersion == "" {
		return nil
	}
	v := &gpuView{index: 0, phys: pg, vg: vg}
	switch h.Mode {
	case topology.ModePassthrough:
		v.kind, v.uuid, v.name = viewPassthrough, pg.g.UUID, pg.g.ModelName
		return v
	case topology.ModeMIG:
		v.kind = viewMIGGuest
	default:
		v.kind = viewVGPU
	}
	mdev := vg.vm.VGPUUUID
	if mdev == "" {
		mdev = topology.StableUUID("mdev", vg.vm.ID)
	}
	v.uuid, v.name = "GPU-"+mdev, vgpuName(vg)
	return v
}

// vgpuProfile is the vGPU type of the VM: the GPU profile on VGPU nodes and the
// MIG-backed vGPU type of the instance on MIG nodes. nil if unknown.
func vgpuProfile(vg *vmGPU) *topology.VGPUProfile {
	if vg.mig != nil {
		return vg.mig.VGPU
	}
	return vg.phys.g.Profile
}

func vgpuName(vg *vmGPU) string {
	if p := vgpuProfile(vg); p != nil && p.Name != "" {
		return p.Name
	}
	if vg.mig != nil {
		return vg.phys.g.ModelName + " " + vg.mig.Profile
	}
	return vg.phys.g.ModelName
}

// vgpuFramebuffer is the framebuffer of the vGPU in MiB.
func vgpuFramebuffer(vg *vmGPU) float64 {
	if p := vgpuProfile(vg); p != nil && p.FramebufferMB > 0 {
		return float64(p.FramebufferMB)
	}
	if vg.mig != nil {
		return float64(vg.mig.MemoryMiB)
	}
	return float64(vg.phys.g.MemoryMiB)
}

// step advances the utilization of the GPU and its VMs for one collection.
//   - vGPU is time sliced: each VM's util is its share of the physical GPU, so the
//     shares are scaled down when the demand exceeds 100.
//   - MIG instances are independent; the device util is the slice weighted mean.
//   - passthrough: the single VM owns the GPU.
func (pg *physGPU) step() {
	pg.util, pg.fbUsed, pg.decUtil = 0, 0, 0
	switch pg.host.Mode {
	case topology.ModeVGPU:
		sum := 0.0
		for _, vg := range pg.vms {
			vg.util = vg.demand.next(vg.r)
			sum += vg.util
		}
		scale := 1.0
		if sum > 100 {
			scale = 100 / sum
		}
		for _, vg := range pg.vms {
			vg.util = round(vg.util*scale, 0)
			pg.util += vg.util
			pg.fbUsed += vgpuFramebuffer(vg)
		}
	case topology.ModeMIG:
		weighted := 0.0
		for _, vg := range pg.vms {
			vg.util = round(vg.demand.next(vg.r), 1)
			slices := 1
			if vg.mig != nil {
				slices = vg.mig.Slices
				pg.fbUsed += float64(vg.mig.MemoryMiB)
			}
			weighted += vg.util * float64(slices)
		}
		pg.util = weighted / migSlices
	case topology.ModePassthrough:
		for _, vg := range pg.vms {
			vg.util = round(vg.demand.next(vg.r), 0)
			pg.util = vg.util
		}
	}
	total := float64(pg.g.MemoryMiB)
	pg.util = clamp(pg.util, 0, 100)
	for _, vg := range pg.vms {
		fb := vgpuFramebuffer(vg)
		if pg.host.Mode == topology.ModePassthrough {
			fb = total
		}
		vg.fbUsed = round(vg.fbFrac.next(vg.r)*fb, 0)
		d := round(vg.dec.next(vg.r)*vg.util/100, 0)
		pg.decUtil += d
	}
	if pg.host.Mode == topology.ModePassthrough && len(pg.vms) > 0 {
		pg.fbUsed = pg.vms[0].fbUsed
	}
	pg.fbUsed = clamp(round(pg.fbUsed, 0), 0, total)
	pg.decUtil = clamp(pg.decUtil, 0, 100)

	r := pg.r
	target := pg.spec.idleTemp + pg.util*0.45
	pg.temp += (target-pg.temp)*0.35 + r.NormFloat64()*0.6
	pg.power = round(pg.spec.tdp*(0.2+0.75*pg.util/100)+r.NormFloat64()*0.3, 2)
	if pg.pstate() == 0 {
		pg.smClock = round(pg.spec.smBoost*(0.92+0.08*r.Float64()), 0)
		pg.memClock = pg.spec.memBusy
		pg.reasons = 0
		if pg.util > 90 && r.Float64() < 0.3 {
			pg.reasons = 4 // SW power cap
		}
	} else {
		pg.smClock, pg.memClock = pg.spec.smIdle, pg.spec.memIdle
		pg.reasons = 1 // GPU idle
	}
}

// pstate is the performance state number. A MIG enabled GPU stays at P0.
func (pg *physGPU) pstate() int {
	if pg.host.Mode == topology.ModeMIG || pg.util >= 3 {
		return 0
	}
	return 8
}

// smiValue is one nvidia-smi query field with the unit the receiver gives it.
type smiValue struct {
	field, unit string
	v           float64
}

// smiValues are the default query fields nvidia-smi answers with a number for
// this view, in the receiver's default field order. A field nvidia-smi answers
// with [N/A] is left out, as the receiver leaves it out:
//   - utilization.* on a MIG enabled GPU and in a MIG-backed vGPU guest.
//   - temperature, power, fan, clock event reasons, ECC, retired pages and
//     remapped rows in a vGPU guest, which does not own the board.
//   - temperature.memory without HBM, fan.speed on passively cooled boards,
//     retired_pages.* from Ampere on (row remapping replaced it) and
//     remapped_rows.* before Ampere.
func (v *gpuView) smiValues() []smiValue {
	pg := v.phys
	board := v.kind == viewHost || v.kind == viewPassthrough
	withUtil := v.kind == viewVGPU || v.kind == viewPassthrough || (v.kind == viewHost && pg.host.Mode != topology.ModeMIG)

	util, total, used, dec := pg.util, float64(pg.g.MemoryMiB), pg.fbUsed, pg.decUtil
	switch v.kind {
	case viewVGPU, viewMIGGuest:
		util, total, used = v.vg.util, vgpuFramebuffer(v.vg), v.vg.fbUsed
		dec = round(v.vg.dec.v*v.vg.util/100, 0)
	case viewPassthrough:
		used = v.vg.fbUsed
	}
	used = clamp(used, 0, total)

	var out []smiValue
	add := func(field, unit string, val float64) { out = append(out, smiValue{field, unit, val}) }
	if withUtil {
		add("utilization.gpu", "%", round(util, 0))
		add("utilization.memory", "%", round(util*0.4, 0))
		add("utilization.encoder", "%", 0)
		add("utilization.decoder", "%", dec)
	}
	add("memory.total", "MiBy", total)
	add("memory.used", "MiBy", used)
	add("memory.free", "MiBy", total-used)
	if board {
		add("temperature.gpu", "Cel", round(pg.temp, 0))
		if pg.spec.hbm {
			add("temperature.memory", "Cel", round(pg.temp+4, 0))
		}
		add("power.draw", "W", pg.power)
		if pg.spec.fan {
			add("fan.speed", "%", round(clamp(30+pg.util*0.5, 0, 100), 0))
		}
	}
	add("clocks.sm", "MHz", pg.smClock)
	add("clocks.mem", "MHz", pg.memClock)
	add("pstate", "", float64(pg.pstate()))
	if !board {
		return out
	}
	add("clocks_event_reasons.active", "", pg.reasons)
	add("ecc.errors.corrected.volatile.total", "", 0)
	add("ecc.errors.uncorrected.volatile.total", "", 0)
	add("ecc.errors.corrected.aggregate.total", "", pg.eccAgg)
	add("ecc.errors.uncorrected.aggregate.total", "", 0)
	switch pg.g.Arch {
	case "Turing", "Volta", "Pascal":
		add("retired_pages.double_bit.count", "", 0)
		add("retired_pages.pending", "", 0)
	default:
		add("remapped_rows.correctable", "", 0)
		add("remapped_rows.uncorrectable", "", 0)
		add("remapped_rows.pending", "", 0)
		add("remapped_rows.failure", "", 0)
	}
	return out
}

// gpmTarget is one device or MIG instance GPM measures.
type gpmTarget struct {
	util    float64
	mig     bool
	gi      int
	migUUID string
}

// gpmTargets are what NVML GPM measures for this view. GPM needs Hopper or
// later. The hypervisor sees the device and, on a MIG node, every instance; a
// passthrough guest sees the device; a vGPU guest gets NOT_SUPPORTED for the
// device and sees only its own MIG instance, numbered 0 inside the guest.
func (v *gpuView) gpmTargets() []gpmTarget {
	pg := v.phys
	if !pg.spec.gpm {
		return nil
	}
	switch v.kind {
	case viewPassthrough:
		return []gpmTarget{{util: pg.util}}
	case viewMIGGuest:
		return []gpmTarget{{util: v.vg.util, mig: true, gi: 0, migUUID: migUUID(pg, v.vg.vm.MIGGPUInstanceID)}}
	case viewVGPU:
		return nil
	}
	out := []gpmTarget{{util: pg.util}}
	for _, mi := range pg.g.MIGInstances {
		u := 0.0
		for _, vg := range pg.vms {
			if vg.mig == mi {
				u = vg.util
			}
		}
		out = append(out, gpmTarget{util: u, mig: true, gi: mi.GPUInstanceID, migUUID: migUUID(pg, mi.GPUInstanceID)})
	}
	return out
}

func migUUID(pg *physGPU, gi int) string {
	return "MIG-" + topology.StableUUID("mig", pg.g.UUID, strconv.Itoa(gi))
}

// gpmFields are the GPM fields the NVML helper reports, all percent, in the order the receiver writes them.
// graphics utilization stays 0 under CUDA load on MIG, as observed on real hardware.
var gpmFields = []string{
	"gpm.dram.bandwidth.utilization",
	"gpm.fp16.utilization",
	"gpm.fp32.utilization",
	"gpm.fp64.utilization",
	"gpm.graphics.utilization",
	"gpm.sm.occupancy",
	"gpm.sm.utilization",
}

func gpmValue(field string, u float64, migMode bool, r *rand.Rand) float64 {
	j := 0.9 + 0.2*r.Float64()
	switch field {
	case "gpm.sm.utilization":
		return round(u, 2)
	case "gpm.sm.occupancy":
		return round(clamp(u*0.55*j, 0, 100), 2)
	case "gpm.graphics.utilization":
		if migMode {
			return 0
		}
		return round(clamp(u*0.95*j, 0, 100), 2)
	case "gpm.dram.bandwidth.utilization":
		return round(clamp(u*0.4*j, 0, 100), 2)
	case "gpm.fp32.utilization":
		return round(clamp(u*0.35*j, 0, 100), 2)
	case "gpm.fp16.utilization":
		return round(clamp(u*0.2*j, 0, 100), 2)
	case "gpm.fp64.utilization":
		return round(clamp(u*0.02*j, 0, 100), 2)
	}
	return 0
}

// emitGPU writes what the GPU receiver sends: nvidia_smi.<field> gauges with
// index, uuid and name, the GPM gauges nvml.gpm.<field> (MIG instances add
// mig, gpu_instance_id and mig.uuid), and the XID counts.
func (a *agent) emitGPU(rm *otlp.ResourceMetrics, e *emitter) {
	if len(a.gpus) == 0 && len(a.xidOrder) == 0 {
		return
	}
	sc := newScope(rm, gpuScope)
	ids := func(v *gpuView) []otlp.KeyValue {
		return []otlp.KeyValue{
			otlp.String("index", strconv.Itoa(v.index)),
			otlp.String("uuid", v.uuid),
			otlp.String("name", v.name),
		}
	}
	for _, v := range a.gpus {
		for _, sv := range v.smiValues() {
			e.double(sc.gauge("nvidia_smi."+sv.field, sv.unit), sv.v, ids(v)...)
		}
	}
	for _, v := range a.gpus {
		migMode := v.phys.host.Mode == topology.ModeMIG
		for _, t := range v.gpmTargets() {
			for _, f := range gpmFields {
				attrs := ids(v)
				if t.mig {
					attrs = append(attrs, otlp.String("mig", "true"), otlp.Int("gpu_instance_id", int64(t.gi)))
					if t.migUUID != "" {
						attrs = append(attrs, otlp.String("mig.uuid", t.migUUID))
					}
				}
				e.double(sc.gauge("nvml."+f, "%"), gpmValue(f, t.util, migMode, v.phys.gpmR), attrs...)
			}
		}
	}
	if len(a.xidOrder) > 0 {
		m := sc.sum("nvidia_kmsg.xid.errors", "{error}", true)
		for _, k := range a.xidOrder {
			e.int(m, a.xid[k], otlp.Int("xid", k.xid), otlp.String("uuid", k.uuid))
		}
	}
}

// xidCodes are common XID numbers with their relative frequency.
var xidCodes = []struct {
	code   int64
	weight int
}{
	{13, 20}, {31, 25}, {43, 20}, {48, 3}, {63, 8}, {79, 2}, {94, 12}, {119, 7},
}

func pickXID(r *rand.Rand) int64 {
	total := 0
	for _, c := range xidCodes {
		total += c.weight
	}
	n := r.Intn(total)
	for _, c := range xidCodes {
		if n < c.weight {
			return c.code
		}
		n -= c.weight
	}
	return xidCodes[len(xidCodes)-1].code
}

// xidEvents rolls an XID per GPU VM and counts it on the agent whose kernel
// logs it: a passthrough guest's own kernel, and for vGPU and MIG VMs the
// hypervisor's, against the physical GPU. The count is by the physical GPU's
// uuid, which is also the uuid a passthrough guest sees.
func (g *Generator) xidEvents() {
	for _, vg := range g.vgpus {
		if vg.xr.Float64() >= g.opt.XIDRate {
			continue
		}
		code := pickXID(vg.xr)
		target := vg.phys.hostAgent
		if vg.phys.host.Mode == topology.ModePassthrough {
			target = nil
			if len(vg.agent.gpus) > 0 {
				target = vg.agent
			}
		}
		if target == nil {
			continue
		}
		target.countXID(xidKey{uuid: vg.phys.g.UUID, xid: code})
	}
}

func (a *agent) countXID(k xidKey) {
	if _, ok := a.xid[k]; !ok {
		a.xidOrder = append(a.xidOrder, k)
		slices.SortFunc(a.xidOrder, func(x, y xidKey) int {
			if c := strings.Compare(x.uuid, y.uuid); c != 0 {
				return c
			}
			return int(x.xid - y.xid)
		})
	}
	a.xid[k]++
}
