package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// gpuSpec holds the model dependent ranges used to make values plausible.
type gpuSpec struct {
	tdp      float64 // board power limit (W)
	idleTemp float64 // idle GPU temperature (C)
	smBoost  float64 // SM clock under load (MHz)
	smIdle   float64 // SM clock at P8 (MHz)
	memBusy  float64 // memory clock under load (MHz)
	memIdle  float64 // memory clock at P8 (MHz)
	hbm      bool    // HBM boards report memory_temp
}

func specFor(model string) gpuSpec {
	m := strings.ToUpper(model)
	switch {
	case strings.Contains(m, "T4"):
		return gpuSpec{70, 34, 1590, 300, 5000, 405, false}
	case strings.Contains(m, "A100") && strings.Contains(m, "PCIE"):
		return gpuSpec{250, 33, 1410, 1410, 1215, 1215, true}
	case strings.Contains(m, "A100"):
		return gpuSpec{400, 32, 1410, 1410, 1593, 1593, true}
	case strings.Contains(m, "H100"), strings.Contains(m, "H200"):
		return gpuSpec{700, 31, 1980, 1980, 2619, 2619, true}
	case strings.Contains(m, "A10"):
		return gpuSpec{150, 36, 1695, 210, 6251, 405, false}
	case strings.Contains(m, "L40"):
		return gpuSpec{350, 35, 2520, 210, 9001, 405, false}
	case strings.Contains(m, "RTX PRO 6000"):
		return gpuSpec{600, 30, 2430, 180, 12481, 405, false}
	}
	return gpuSpec{300, 34, 1500, 300, 5000, 405, false}
}

// step advances the utilization of the GPU and its VMs for one collection.
//   - vGPU is time sliced: each VM's util is its share of the physical GPU, so the
//     shares are scaled down when the demand exceeds 100.
//   - MIG instances are independent; the device util is the slice weighted mean.
//   - passthrough: the single VM owns the GPU.
func (gs *gpuState) step() {
	mode := gs.mode
	gs.util = 0
	gs.fbUsed = 0
	switch mode {
	case topology.ModeVGPU:
		sum := 0.0
		for _, vs := range gs.vms {
			vs.util = vs.demand.next(vs.r)
			sum += vs.util
		}
		scale := 1.0
		if sum > 100 {
			scale = 100 / sum
		}
		for _, vs := range gs.vms {
			vs.util = round(vs.util*scale, 0)
			gs.util += vs.util
			gs.fbUsed += float64(vgpuFramebuffer(vs))
		}
		gs.util = clamp(gs.util, 0, 100)
		gs.fbUsed = clamp(gs.fbUsed, 0, float64(gs.g.MemoryMiB)*0.99)
	case topology.ModeMIG:
		weighted := 0.0
		for _, vs := range gs.vms {
			vs.util = round(vs.demand.next(vs.r), 1)
			slices, mem := 1, 0
			if vs.mig != nil {
				slices, mem = vs.mig.Slices, vs.mig.MemoryMiB
			}
			weighted += vs.util * float64(slices)
			gs.fbUsed += float64(mem)
		}
		gs.util = clamp(weighted/float64(topology.MIGSlices(gs.g.ModelName)), 0, 100)
		gs.fbUsed = clamp(gs.fbUsed, 0, float64(gs.g.MemoryMiB))
	case topology.ModePassthrough:
		if len(gs.vms) > 0 {
			vs := gs.vms[0]
			vs.util = round(vs.demand.next(vs.r), 0)
			gs.util = vs.util
			gs.fbUsed = round(vs.fbFrac.next(vs.r)*float64(gs.g.MemoryMiB), 0)
		}
	}
	target := gs.spec.idleTemp + gs.util*0.45
	gs.temp += (target-gs.temp)*0.35 + gs.r.NormFloat64()*0.6
}

// pstate is the performance state number. A MIG enabled GPU stays at P0.
func (gs *gpuState) pstate() int {
	if gs.mode == topology.ModeMIG || gs.util >= 3 {
		return 0
	}
	return 8
}

// physFields are the dcgm fields the nvidia_smi_to_dcgm processor produces from one
// nvidia-smi GPU: everything float except pstate (integer), and no server_time because
// the processor clears the original fields. withUtil is false on MIG enabled GPUs where
// nvidia-smi reports utilization as N/A.
func (gs *gpuState) physFields(withUtil bool) map[string]any {
	r := gs.r
	ps := gs.pstate()
	total := float64(gs.g.MemoryMiB)
	used := clamp(round(gs.fbUsed, 0), 0, total)
	f := map[string]any{
		"gpu_temp":            round(gs.temp, 0),
		"power_usage":         round(gs.spec.tdp*(0.2+0.75*gs.util/100)+r.NormFloat64()*0.3, 2),
		"fb_total":            total,
		"fb_used":             used,
		"fb_free":             total - used,
		"pstate":              int64(ps),
		"pcie_replay_counter": 0.0,
		"ecc_sbe_vol_total":   0.0,
		"ecc_dbe_vol_total":   0.0,
		"ecc_sbe_agg_total":   0.0,
		"ecc_dbe_agg_total":   0.0,
	}
	if ps == 0 {
		f["sm_clock"] = round(gs.spec.smBoost*(0.92+0.08*r.Float64()), 0)
		f["mem_clock"] = gs.spec.memBusy
		reasons := 0.0
		if gs.util > 90 && r.Float64() < 0.3 {
			reasons = 4 // SW power cap
		}
		f["clocks_event_reasons"] = reasons
	} else {
		f["sm_clock"] = gs.spec.smIdle
		f["mem_clock"] = gs.spec.memIdle
		f["clocks_event_reasons"] = 1.0 // GPU idle
	}
	if withUtil {
		f["gpu_util"] = round(gs.util, 0)
		f["mem_copy_util"] = round(gs.util*0.4, 0)
		f["enc_util"] = 0.0
		f["dec_util"] = 0.0
	}
	if gs.spec.hbm {
		f["memory_temp"] = round(gs.temp+4, 0)
	}
	switch gs.g.Arch {
	case "Turing", "Volta", "Pascal":
		f["retired_pages_dbe"] = 0.0
		f["retired_pages_pending"] = 0.0
	default:
		f["correctable_remapped_rows"] = 0.0
		f["uncorrectable_remapped_rows"] = 0.0
		f["row_remap_failure"] = 0.0
		f["row_remap_pending"] = 0.0
	}
	return f
}

// physTags are the tags of a dcgm point that came from nvidia-smi through
// nvidia_smi_to_dcgm: the plugin tags (index, pstate, name, arch, compute_mode,
// pci_bus_id) stay, uuid becomes UUID, the agent uuid is restored, index is copied to
// gpu, name to modelName, pci_bus_id to DCGM_FI_DEV_PCI_BUSID, and the version string
// fields become DCGM_FI_* tags.
func physTags(base map[string]string, gs *gpuState, index int, busID, driver, cuda string) map[string]string {
	idx := strconv.Itoa(index)
	return copyTags(base,
		"index", idx,
		"gpu", idx,
		"pstate", "P"+strconv.Itoa(gs.pstate()),
		"name", gs.g.ModelName,
		"modelName", gs.g.ModelName,
		"arch", gs.g.Arch,
		"UUID", gs.g.UUID,
		"compute_mode", "Default",
		"pci_bus_id", busID,
		"DCGM_FI_DEV_PCI_BUSID", busID,
		"DCGM_FI_DRIVER_VERSION", driver,
		"DCGM_FI_DEV_VBIOS_VERSION", gs.g.VBIOS,
		"DCGM_FI_NVML_VERSION", nvmlVersion(driver),
		"DCGM_FI_CUDA_DRIVER_VERSION", cuda,
	)
}

// emitPhysical writes the physical GPU point of a VGPU or MIG GPU, and for a MIG GPU
// the GPM points (device and per instance) that gpm_to_vm moves onto
// the physical and VM series.
func (g *Generator) emitPhysical(b *batch, hs *hostState, gs *gpuState, ts, st int64) {
	base := g.hostTags(hs)
	mig := gs.mode == topology.ModeMIG
	// The vGPU host driver reports no CUDA version ("Not Found"), so no CUDA tag here.
	tags := physTags(base, gs, gs.g.Index, gs.g.BusID, hs.h.DriverVersion, "")
	b.add("dcgm", tags, gs.physFields(!mig), ts)
	if !mig {
		return
	}
	dev := gpmFields(gs.util, gs.r)
	dev["server_time"] = st
	b.add("dcgm", tags, dev, ts)
	for _, vs := range gs.vms {
		if vs.mig == nil {
			continue
		}
		f := gpmFields(vs.util, vs.r)
		f["server_time"] = st
		b.add("dcgm", g.vmExecTags(vs), f, ts)
	}
}

// gpmFields are the nvidia_gpm fields after gpm_to_vm renamed sm_util to gpu_util.
// graphics_util stays 0 under CUDA load on MIG, as observed on real hardware.
func gpmFields(u float64, r interface{ Float64() float64 }) map[string]any {
	j := func() float64 { return 0.9 + 0.2*r.Float64() }
	return map[string]any{
		"gpu_util":      round(u, 2),
		"sm_occupancy":  round(clamp(u*0.55*j(), 0, 100), 2),
		"graphics_util": 0.0,
		"dram_bw_util":  round(clamp(u*0.4*j(), 0, 100), 2),
		"fp32_util":     round(clamp(u*0.35*j(), 0, 100), 2),
		"fp16_util":     round(clamp(u*0.2*j(), 0, 100), 2),
		"fp64_util":     round(clamp(u*0.02*j(), 0, 100), 2),
	}
}

// vmExecTags are the tags gpu_vm_mapping.sh puts on the dcgm point it writes for each
// vGPU (or MIG backed vGPU) VM from the hypervisor. The script sets uuid, type=vm, host
// and gpu_type itself, so the hypervisor global tags only fill time_diff; there is no
// project_id on these points.
func (g *Generator) vmExecTags(vs *vmState) map[string]string {
	gp := vs.gpu.g
	gpuType := "VGPU"
	gi := "N/A"
	if vs.gpu.mode == topology.ModeMIG {
		gpuType = "MIG"
		if vs.vm.MIGGPUInstanceID >= 0 {
			gi = strconv.Itoa(vs.vm.MIGGPUInstanceID)
		}
	}
	driver := vs.vm.GuestDriverVersion
	if driver == "" {
		driver = vs.host.h.DriverVersion
	}
	return map[string]string{
		"UUID":                      vs.vm.VGPUUUID,
		"gpu":                       strconv.Itoa(vs.vgpuIndex),
		"gpu_type":                  gpuType,
		"host":                      vs.vm.Name,
		"modelName":                 vgpuName(vs),
		"pci_bus_id":                gp.BusID,
		"DCGM_FI_DEV_PCI_BUSID":     gp.BusID,
		"DCGM_FI_DRIVER_VERSION":    driver,
		"DCGM_FI_DEV_VBIOS_VERSION": gp.VBIOS,
		"uuid":                      g.vmUUID(vs.vm),
		"credential_id":             g.opt.CredentialID,
		"cloud_service":             "openstack",
		"collection_type":           "agent",
		"type":                      "vm",
		"gpu_instance_id":           gi,
		"physical_gpu_uuid":         gp.UUID,
		"time_diff":                 "0",
	}
}

// vgpuProfile returns the vGPU type of the VM: the GPU profile on a VGPU GPU and the
// MIG backed vGPU type of the instance on a MIG GPU. nil if unknown.
func vgpuProfile(vs *vmState) *topology.VGPUProfile {
	if vs.mig != nil {
		return vs.mig.VGPU
	}
	return vs.gpu.g.Profile
}

func vgpuName(vs *vmState) string {
	if p := vgpuProfile(vs); p != nil && p.Name != "" {
		return p.Name
	}
	if vs.mig != nil {
		return vs.gpu.g.ModelName + " " + vs.mig.Profile
	}
	return vs.gpu.g.ModelName
}

// vgpuFramebuffer is the framebuffer of the vGPU as reported by `nvidia-smi vgpu -q` (MiB).
func vgpuFramebuffer(vs *vmState) int {
	if p := vgpuProfile(vs); p != nil && p.FramebufferMB > 0 {
		return p.FramebufferMB
	}
	if vs.mig != nil {
		return vs.mig.MemoryMiB
	}
	return vs.gpu.g.MemoryMiB
}

// emitMapping writes what gpu_vm_mapping.sh prints on one hypervisor, in its order:
// per vGPU instance a gpu_vm_mapping point followed by a dcgm point for the VM, then
// passthrough GPUs found in libvirt hostdevs, then every GPU that was not assigned.
// The script judges each GPU on its own, so gpu_type is the mode of that GPU.
// A MIG GPU with at least one assigned instance is "assigned" as a whole, so its
// unused instances produce no row.
func (g *Generator) emitMapping(b *batch, hs *hostState, ts, st int64) {
	base := g.hostTags(hs)
	scriptType := func(gs *gpuState) string { return strings.ToLower(string(gs.mode)) }
	assigned := map[int]bool{}

	for _, gs := range hs.gpus {
		if gs.mode == topology.ModeVGPU || gs.mode == topology.ModeMIG {
			for _, vs := range gs.vms {
				assigned[gs.g.Index] = true
				p := vgpuProfile(vs)
				profType, maxInst, fb := "unknown", 0, vgpuFramebuffer(vs)
				if p != nil {
					if p.Type != "" {
						profType = p.Type
					}
					maxInst = p.MaxInstances
				}
				tags := copyTags(base,
					"openstack_vm_name", orNA(vs.vm.Name),
					"openstack_vm_uuid", orNA(vs.vm.ID),
					"host_bdf", gs.g.HostBDF,
					"guest_bdf", "vgpu",
					"vendor_id", "10de",
					"device_id", orDefault(gs.g.DeviceID, "0000"),
					"status", "assigned",
					"driver", "nvidia",
					"gpu_type", scriptType(gs),
					"vgpu_uuid", vs.vm.VGPUUUID,
				)
				b.add("gpu_vm_mapping", tags, map[string]any{
					"mapped":                int64(1),
					"vgpu_profile":          profType,
					"vgpu_model_name":       vgpuName(vs),
					"gpu_uuid":              gs.g.UUID,
					"vgpu_fb_mb":            int64(fb),
					"vgpu_max_instances":    int64(maxInst),
					"vgpu_active_instances": int64(len(gs.vms)),
					"server_time":           st,
				}, ts)
				b.add("dcgm", g.vmExecTags(vs), g.vmExecFields(vs, fb, st), ts)
			}
		}
	}

	for _, gs := range hs.gpus {
		if gs.mode == topology.ModePassthrough {
			if len(gs.vms) == 0 {
				continue
			}
			vs := gs.vms[0]
			assigned[gs.g.Index] = true
			// The hypervisor has no nvidia driver, so the script knows no GPU UUID here.
			// The monitoring server fills it from the guest dcgm series (uuid -> UUID).
			tags := copyTags(base,
				"openstack_vm_name", orNA(vs.vm.Name),
				"openstack_vm_uuid", orNA(vs.vm.ID),
				"host_bdf", gs.g.HostBDF,
				"guest_bdf", orDefault(vs.vm.GuestBDF, "00:00.0"),
				"vendor_id", "10de",
				"device_id", gs.g.DeviceID,
				"status", "assigned",
				"driver", "vfio-pci",
				"gpu_type", "passthrough",
			)
			b.add("gpu_vm_mapping", tags, map[string]any{"mapped": int64(1), "server_time": st}, ts)
		}
	}

	for _, gs := range hs.gpus {
		if assigned[gs.g.Index] {
			continue
		}
		driver := "nvidia"
		f := map[string]any{"mapped": int64(0), "vgpu_active_instances": int64(0), "server_time": st}
		if gs.mode == topology.ModePassthrough {
			driver = "vfio-pci"
		} else {
			f["gpu_uuid"] = gs.g.UUID
		}
		tags := copyTags(base,
			"openstack_vm_name", "N/A",
			"openstack_vm_uuid", "N/A",
			"host_bdf", gs.g.HostBDF,
			"guest_bdf", "N/A",
			"vendor_id", "10de",
			"device_id", gs.g.DeviceID,
			"status", "unassigned",
			"driver", driver,
			"gpu_type", scriptType(gs),
		)
		b.add("gpu_vm_mapping", tags, f, ts)
	}
}

// vmExecFields are the fields of the per VM dcgm point from gpu_vm_mapping.sh.
// Utilization is N/A for MIG backed vGPU, so it is left out there (GPM fills it).
func (g *Generator) vmExecFields(vs *vmState, fbTotal int, st int64) map[string]any {
	total := float64(fbTotal)
	used := round(vs.fbFrac.next(vs.r)*total, 0)
	f := map[string]any{
		"fb_total":            total,
		"fb_used":             used,
		"fb_free":             total - used,
		"vgpu_license_status": float64(vs.license),
		"pstate":              int64(vs.gpu.pstate()),
		"server_time":         st,
	}
	if vs.gpu.mode == topology.ModeMIG {
		f["mig_mode"] = 1.0
		return f
	}
	f["mig_mode"] = 0.0
	f["gpu_util"] = vs.util
	f["mem_copy_util"] = round(vs.util*0.4, 0)
	f["enc_util"] = 0.0
	f["dec_util"] = round(vs.dec.next(vs.r)*vs.util/100, 0)
	return f
}

// emitPassthroughGuest writes the dcgm point the agent inside a passthrough VM produces
// with nvidia-smi: the VM sees the whole physical GPU (same GPU- UUID) at its guest PCI
// address, with the guest driver and CUDA version.
func (g *Generator) emitPassthroughGuest(b *batch, vs *vmState, base map[string]string, ts int64) {
	gs := vs.gpu
	if len(gs.vms) == 0 || gs.vms[0] != vs {
		return
	}
	driver := vs.vm.GuestDriverVersion
	tags := physTags(base, gs, 0, guestBusID(vs.vm), driver, cudaVersion(driver))
	b.add("dcgm", tags, gs.physFields(true), ts)
}

// emitCUDAProbe writes the cuda_version.sh output of a vGPU/MIG guest:
// dcgm,UUID=<uuid without GPU->,DCGM_FI_CUDA_DRIVER_VERSION=<x.y> cuda_probe=1i
// plus the guest global tags.
func (g *Generator) emitCUDAProbe(b *batch, vs *vmState, base map[string]string, ts, st int64) {
	cuda := cudaVersion(vs.vm.GuestDriverVersion)
	u := strings.TrimPrefix(vs.vm.VGPUUUID, "GPU-")
	if cuda == "" || u == "" {
		return
	}
	b.add("dcgm", copyTags(base, "UUID", u, "DCGM_FI_CUDA_DRIVER_VERSION", cuda),
		map[string]any{"cuda_probe": int64(1), "server_time": st}, ts)
}

type xidKind struct {
	code    int
	weight  int
	process bool // the kernel line names the faulting process
	text    string
}

// xidKinds are common XID codes with the text the driver prints after
// "NVRM: Xid (PCI:<bdf>): <code>, pid=..., name=..., ".
var xidKinds = []xidKind{
	{13, 20, true, "Graphics SM Warp Exception on (GPC 1, TPC 2, SM 0): Out Of Range Address"},
	{31, 25, true, "Ch 00000010, intr 00000000. MMU Fault: ENGINE GRAPHICS GPCCLIENT_T1_3 faulted @ 0x7f3a_8c000000. Fault is of type FAULT_PDE ACCESS_TYPE_VIRT_READ"},
	{43, 20, true, "Ch 00000018"},
	{48, 3, false, "An uncorrectable double bit error (DBE) has been detected on GPU in the framebuffer at partition 6, subpartition 0."},
	{63, 8, false, "Row Remapper: New row (0x00000000000a3f20) was marked for remapping, reset gpu to activate."},
	{79, 2, false, "GPU has fallen off the bus."},
	{94, 12, true, "Contained: SM (0x1). RST: No, D-RST: No"},
	{119, 7, false, "Timeout after 6s of waiting for RPC response from GPU0 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20800a4c 0x4)."},
}

// emitXID writes the dcgm point the xid processor derives from a kernel "NVRM: Xid" line.
// The processor builds a new metric, so it has none of the agent global tags and no
// server_time/time_diff: only err_code, DCGM_FI_DEV_PCI_BUSID (normalized), uuid, and
// the GPU meta (UUID, modelName, DCGM_FI_DRIVER_VERSION) learned from the dcgm point of
// the same agent and PCI address. err_msg is a string field holding the kernel line.
//
// Only agents that run nvidia-smi have that processor: the passthrough guest itself, and
// for vGPU/MIG VMs the hypervisor, whose kernel logs the XID against the physical GPU.
func (g *Generator) emitXID(b *batch, vs *vmState, now time.Time) {
	r := vs.xr
	k := pickXID(r.Intn(xidWeightTotal()))
	gs := vs.gpu

	var uuid, bus, driver string
	ts := now
	if vs.gpu.mode == topology.ModePassthrough {
		uuid, bus, driver = g.vmUUID(vs.vm), guestBusID(vs.vm), vs.vm.GuestDriverVersion
		ts = now.Add(time.Duration(vs.timeDiff) * time.Second)
	} else {
		uuid, bus, driver = g.hostUUID(vs.host.h), gs.g.BusID, vs.host.h.DriverVersion
	}
	bdf := normBDF(bus)
	if bdf == "" {
		return
	}
	who := "pid=0, name=<unknown>"
	if k.process {
		who = fmt.Sprintf("pid=%d, name=python3", 2000+r.Intn(60000))
	}
	msg := fmt.Sprintf("NVRM: Xid (PCI:%s): %d, %s, %s", bdf, k.code, who, k.text)
	tags := map[string]string{
		"err_code":               strconv.Itoa(k.code),
		"DCGM_FI_DEV_PCI_BUSID":  bdf,
		"uuid":                   uuid,
		"UUID":                   gs.g.UUID,
		"modelName":              gs.g.ModelName,
		"DCGM_FI_DRIVER_VERSION": driver,
	}
	b.add("dcgm", tags, map[string]any{"xid_errors": float64(k.code), "err_msg": msg}, ts.UnixNano())
}

func xidWeightTotal() int {
	n := 0
	for _, k := range xidKinds {
		n += k.weight
	}
	return n
}

func pickXID(n int) xidKind {
	for _, k := range xidKinds {
		if n < k.weight {
			return k
		}
		n -= k.weight
	}
	return xidKinds[len(xidKinds)-1]
}

// normBDF converts a PCI address to the form the xid processor joins on:
// "00000000:3B:00.0" and "PCI:0000:3b:00" both become "0000:3b:00".
func normBDF(raw string) string {
	s := strings.TrimPrefix(strings.ToLower(raw), "pci:")
	if i := strings.Index(s, "."); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 3 {
		return ""
	}
	dom := parts[0]
	if len(dom) > 4 {
		dom = dom[len(dom)-4:]
	}
	return dom + ":" + parts[1] + ":" + parts[2]
}

// guestBusID is the nvidia-smi bus id of the GPU inside the VM ("00000000:00:05.0").
func guestBusID(vm *topology.VM) string {
	if vm.GuestBDF == "" {
		return ""
	}
	return "00000000:" + strings.ToUpper(vm.GuestBDF)
}

// nvmlVersion is the NVML version `nvidia-smi --version` prints for a driver: its
// first two components ("595.91.04" -> "595.91").
func nvmlVersion(driver string) string {
	parts := strings.Split(driver, ".")
	if len(parts) < 2 {
		return driver
	}
	return parts[0] + "." + parts[1]
}

// cudaVersion is the "CUDA Version" nvidia-smi prints inside a guest for a driver branch.
func cudaVersion(driver string) string {
	major, err := strconv.Atoi(strings.SplitN(driver, ".", 2)[0])
	if err != nil {
		return ""
	}
	switch {
	case major >= 595:
		return "13.2"
	case major >= 590:
		return "13.1"
	case major >= 580:
		return "13.0"
	case major >= 575:
		return "12.9"
	case major >= 570:
		return "12.8"
	case major >= 560:
		return "12.6"
	case major >= 550:
		return "12.4"
	case major >= 535:
		return "12.2"
	}
	return "12.0"
}

func orNA(s string) string {
	return orDefault(s, "N/A")
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
