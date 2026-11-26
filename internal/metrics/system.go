package metrics

import (
	"fmt"
	"math/rand"
)

// The general (non-GPU) inputs the monitoring server enables on every agent:
// cpu (percpu + total, usage_guest* excluded), mem (commit*/dirty/huge*/... excluded),
// swap, disk, diskio (weighted_io_time/merged* excluded), net (speed excluded, protocol
// stats off), processes (paging/total_threads excluded), procstat (60s, top 10 by
// cpu/memory, only cpu_usage/memory_usage) and system (uptime_format excluded).
// Every point also carries server_time (int, unix seconds) and the time_diff tag,
// which the agent's first processor adds before anything else runs.

type diskState struct {
	device, fstype, path, label string
	total                       int64
	used                        *walk // fraction of total
	inodes                      int64
}

type diskioState struct {
	name, wwid                  string
	reads, writes               counter
	readBytes, writeBytes       counter
	readTime, writeTime, ioTime counter
}

type netState struct {
	iface                          string
	bytesRecv, bytesSent           counter
	packetsRecv, packetsSent       counter
	errIn, errOut, dropIn, dropOut counter
	rate                           float64 // bytes per second at full load
}

type procSpec struct {
	name, user string
	cpuShare   float64 // share of node cpu load
	mem        float64 // memory_usage percent
}

// nodeState holds the general metric state of one agent (hypervisor or VM).
type nodeState struct {
	r        *rand.Rand
	isVM     bool
	ncpu     int
	load     *walk
	cpuSkew  []float64
	memTotal int64
	memUsed  *walk
	swap     int64
	disks    []*diskState
	dios     []*diskioState
	nets     []*netState
	procs    []procSpec
	uptime   int64
	load1    float64
	load5    float64
	load15   float64
	nprocs   int
}

func newNodeState(r *rand.Rand, isVM bool, ncpu, memMB, diskGB int) *nodeState {
	if ncpu < 1 {
		ncpu = 1
	}
	if diskGB < 1 {
		diskGB = 20
	}
	n := &nodeState{
		r:        r,
		isVM:     isVM,
		ncpu:     ncpu,
		load:     newWalk(r, 0, 90, 2, 0.01),
		memTotal: int64(memMB) * 1024 * 1024,
		memUsed:  newWalk(r, 0.08, 0.85, 0.005, 0.005),
		uptime:   int64(86400 * (3 + r.Intn(120))),
	}
	n.cpuSkew = make([]float64, ncpu)
	for i := range n.cpuSkew {
		n.cpuSkew[i] = r.NormFloat64() * 4
	}
	gib := int64(1024 * 1024 * 1024)
	if isVM {
		n.memTotal = n.memTotal * 97 / 100
		n.disks = []*diskState{
			{device: "vda1", fstype: "ext4", path: "/", label: "cloudimg-rootfs", total: int64(diskGB) * gib},
		}
		n.dios = []*diskioState{{name: "vda"}}
		n.nets = []*netState{{iface: "ens3", rate: 4e7}}
		n.procs = []procSpec{
			{"python3", "root", 0.55, 18},
			{"containerd", "root", 0.04, 0.7},
			{"telegraf", "root", 0.02, 2.5},
			{"systemd-journald", "root", 0.01, 0.4},
			{"sshd", "root", 0.005, 0.1},
		}
		n.nprocs = 120 + r.Intn(80)
	} else {
		n.swap = 8 * gib
		n.disks = []*diskState{
			{device: "nvme0n1p3", fstype: "ext4", path: "/", total: int64(diskGB) * gib},
			{device: "nvme0n1p2", fstype: "ext4", path: "/boot", total: 2 * gib},
		}
		n.dios = []*diskioState{{name: "nvme0n1", wwid: fmt.Sprintf("eui.%016x", r.Uint64())}}
		n.nets = []*netState{{iface: "eno1", rate: 2e8}, {iface: "eno2", rate: 2e7}}
		n.procs = []procSpec{
			{"qemu-system-x86_64", "42436", 0.6, 3.2},
			{"nova-compute", "42436", 0.02, 0.3},
			{"libvirtd", "root", 0.015, 0.1},
			{"ovs-vswitchd", "root", 0.01, 0.2},
			{"telegraf", "root", 0.005, 0.1},
		}
		n.nprocs = 900 + r.Intn(400)
	}
	for _, d := range n.disks {
		d.used = newWalk(r, 0.05, 0.8, 0.0005, 0.001)
		d.inodes = d.total / 16384
	}
	for _, d := range n.dios {
		d.reads.v = float64(r.Int63n(1e7))
		d.writes.v = float64(r.Int63n(1e8))
		d.readBytes.v = d.reads.v * 32768
		d.writeBytes.v = d.writes.v * 16384
		d.readTime.v = d.reads.v / 5
		d.writeTime.v = d.writes.v / 2
		d.ioTime.v = float64(r.Int63n(1e8))
	}
	for _, ni := range n.nets {
		ni.bytesRecv.v = float64(r.Int63n(1e11))
		ni.bytesSent.v = float64(r.Int63n(1e11))
		ni.packetsRecv.v = ni.bytesRecv.v / 1200
		ni.packetsSent.v = ni.bytesSent.v / 1200
	}
	return n
}

// emit writes all general metrics of this node. gpuBusy (0-100) pulls the cpu load up so
// a VM that drives its GPU also looks busy on the cpu side.
func (n *nodeState) emit(b *batch, base map[string]string, ts, serverTime int64, dt, gpuBusy float64, due60 bool) {
	r := n.r
	load := n.load.next(r)
	if gpuBusy > 0 {
		load = clamp(load*0.6+gpuBusy*0.35, 0, 100)
	}
	n.uptime += int64(dt)

	// cpu: one point per cpu plus cpu-total (the mean).
	var sum [8]float64
	for i := 0; i < n.ncpu; i++ {
		u := clamp(load+n.cpuSkew[i]+r.NormFloat64()*3, 0, 100)
		f := n.cpuFields(u, r)
		for j, k := range cpuFieldNames {
			sum[j] += f[k].(float64)
		}
		f["server_time"] = serverTime
		b.add("cpu", copyTags(base, "cpu", fmt.Sprintf("cpu%d", i)), f, ts)
	}
	total := map[string]any{"server_time": serverTime}
	for j, k := range cpuFieldNames {
		total[k] = round(sum[j]/float64(n.ncpu), 6)
	}
	b.add("cpu", copyTags(base, "cpu", "cpu-total"), total, ts)

	// mem
	usedFrac := n.memUsed.next(r)
	used := int64(float64(n.memTotal) * usedFrac)
	cached := int64(float64(n.memTotal-used) * 0.8)
	free := n.memTotal - used - cached
	buffered := n.memTotal / 200
	available := n.memTotal - used
	b.add("mem", base, map[string]any{
		"total":             n.memTotal,
		"used":              used,
		"free":              free,
		"cached":            cached,
		"buffered":          buffered,
		"available":         available,
		"active":            used + cached/4,
		"inactive":          cached * 3 / 4,
		"shared":            n.memTotal / 100,
		"used_percent":      round(float64(used)*100/float64(n.memTotal), 6),
		"available_percent": round(float64(available)*100/float64(n.memTotal), 6),
		"server_time":       serverTime,
	}, ts)

	// swap: hypervisors have a small swap, cloud images have none.
	swapUsed := int64(0)
	swapPct := 0.0
	if n.swap > 0 {
		swapUsed = n.swap / 1000
		swapPct = round(float64(swapUsed)*100/float64(n.swap), 6)
	}
	b.add("swap", base, map[string]any{
		"total":        n.swap,
		"used":         swapUsed,
		"free":         n.swap - swapUsed,
		"used_percent": swapPct,
		"server_time":  serverTime,
	}, ts)

	// disk
	for _, d := range n.disks {
		u := int64(float64(d.total) * d.used.next(r))
		iu := d.inodes / 100
		tags := copyTags(base, "device", d.device, "fstype", d.fstype, "mode", "rw", "path", d.path)
		if d.label != "" {
			tags["label"] = d.label
		}
		b.add("disk", tags, map[string]any{
			"total":               d.total,
			"used":                u,
			"free":                d.total - u,
			"used_percent":        round(float64(u)*100/float64(d.total), 6),
			"inodes_total":        d.inodes,
			"inodes_used":         iu,
			"inodes_free":         d.inodes - iu,
			"inodes_used_percent": round(float64(iu)*100/float64(d.inodes), 6),
			"server_time":         serverTime,
		}, ts)
	}

	// diskio: cumulative counters grow with load.
	act := 0.1 + load/100
	for _, d := range n.dios {
		reads := d.reads.add(r, 40*act, dt)
		writes := d.writes.add(r, 120*act, dt)
		tags := copyTags(base, "name", d.name)
		if d.wwid != "" {
			tags["wwid"] = d.wwid
		}
		b.add("diskio", tags, map[string]any{
			"reads":            reads,
			"writes":           writes,
			"read_bytes":       d.readBytes.add(r, 40*act*32768, dt),
			"write_bytes":      d.writeBytes.add(r, 120*act*16384, dt),
			"read_time":        d.readTime.add(r, 8*act, dt),
			"write_time":       d.writeTime.add(r, 60*act, dt),
			"io_time":          d.ioTime.add(r, 1000*act*0.05, dt),
			"iops_in_progress": int64(0),
			"io_await":         round(0.2+r.Float64()*0.8, 6),
			"io_svctm":         round(0.1+r.Float64()*0.2, 6),
			"io_util":          round(act*5*r.Float64(), 6),
			"server_time":      serverTime,
		}, ts)
	}

	// net
	for _, ni := range n.nets {
		rate := ni.rate * (0.05 + load/100)
		b.add("net", copyTags(base, "interface", ni.iface), map[string]any{
			"bytes_recv":   ni.bytesRecv.add(r, rate, dt),
			"bytes_sent":   ni.bytesSent.add(r, rate*0.6, dt),
			"packets_recv": ni.packetsRecv.add(r, rate/1200, dt),
			"packets_sent": ni.packetsSent.add(r, rate*0.6/1200, dt),
			"err_in":       int64(ni.errIn.v),
			"err_out":      int64(ni.errOut.v),
			"drop_in":      ni.dropIn.bump(r, 0.002),
			"drop_out":     int64(ni.dropOut.v),
			"server_time":  serverTime,
		}, ts)
	}

	// system: load averages follow the cpu load with the usual lag.
	runq := load / 100 * float64(n.ncpu)
	n.load1 += (runq - n.load1) * 0.5
	n.load5 += (runq - n.load5) * 0.15
	n.load15 += (runq - n.load15) * 0.05
	sys := map[string]any{
		"load1":           round(n.load1, 2),
		"load5":           round(n.load5, 2),
		"load15":          round(n.load15, 2),
		"n_cpus":          int64(n.ncpu),
		"n_physical_cpus": int64(n.physicalCPUs()),
		"uptime":          n.uptime,
		"server_time":     serverTime,
	}
	if n.isVM {
		sys["n_users"] = int64(1)
		sys["n_unique_users"] = int64(1)
	}
	b.add("system", base, sys, ts)

	// processes
	running := int64(1 + int(load/100*float64(n.ncpu)/4))
	sleeping := int64(n.nprocs) * 6 / 10
	idle := int64(n.nprocs) - sleeping - running
	b.add("processes", base, map[string]any{
		"blocked":     int64(0),
		"dead":        int64(0),
		"idle":        idle,
		"running":     running,
		"sleeping":    sleeping,
		"stopped":     int64(0),
		"total":       int64(n.nprocs),
		"unknown":     int64(0),
		"zombies":     int64(0),
		"server_time": serverTime,
	}, ts)

	// procstat runs on a 60s interval.
	if due60 {
		for _, p := range n.procs {
			cpu := load * float64(n.ncpu) * p.cpuShare * (0.8 + 0.4*r.Float64())
			b.add("procstat", copyTags(base, "process_name", p.name, "user", p.user, "filter", "exclude_empty_cmdline"),
				map[string]any{
					"cpu_usage":    round(cpu, 6),
					"memory_usage": round(p.mem*(0.95+0.1*r.Float64()), 6),
					"server_time":  serverTime,
				}, ts)
		}
	}
}

var cpuFieldNames = [8]string{
	"usage_user", "usage_system", "usage_idle", "usage_nice",
	"usage_iowait", "usage_irq", "usage_softirq", "usage_steal",
}

func (n *nodeState) cpuFields(u float64, r *rand.Rand) map[string]any {
	steal := 0.0
	if n.isVM {
		steal = u * 0.01 * r.Float64()
	}
	user := u * 0.62
	system := u * 0.28
	nice := u * 0.01
	iowait := u * 0.03 * r.Float64()
	irq := u * 0.01
	softirq := u * 0.03
	idle := clamp(100-(user+system+nice+iowait+irq+softirq+steal), 0, 100)
	return map[string]any{
		"usage_user":    round(user, 6),
		"usage_system":  round(system, 6),
		"usage_idle":    round(idle, 6),
		"usage_nice":    round(nice, 6),
		"usage_iowait":  round(iowait, 6),
		"usage_irq":     round(irq, 6),
		"usage_softirq": round(softirq, 6),
		"usage_steal":   round(steal, 6),
	}
}

func (n *nodeState) physicalCPUs() int {
	if n.isVM || n.ncpu < 2 {
		return n.ncpu
	}
	return n.ncpu / 2
}
