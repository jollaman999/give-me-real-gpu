package otelgen

import (
	"crypto/sha1"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/otlp"
	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// scraperScope is the instrumentation scope the hostmetrics receiver gives a scraper's metrics.
func scraperScope(name string) string {
	return "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/hostmetricsreceiver/internal/scraper/" + name + "scraper"
}

const (
	kib = 1024.0
	mib = 1024 * kib
	gib = 1024 * mib
)

// cpuStates are the state values of system.cpu.time on Linux.
var cpuStates = []string{"user", "system", "idle", "interrupt", "nice", "softirq", "steal", "wait"}

// tcpStates are the state values of system.network.connections for tcp.
var tcpStates = []string{"CLOSE", "CLOSE_WAIT", "CLOSING", "DELETE", "ESTABLISHED", "FIN_WAIT_1",
	"FIN_WAIT_2", "LAST_ACK", "LISTEN", "SYN_RECV", "SYN_SENT", "TIME_WAIT"}

// sysState is what the hostmetrics scrapers read on one node.
type sysState struct {
	r        *rand.Rand
	isVM     bool
	vcpus    int
	physCPUs int
	memTotal float64 // bytes

	load       *walk // share of all CPUs busy
	cpuBias    []float64
	cpuTime    [][]float64 // per CPU, per cpuStates entry (s)
	load1      float64
	load5      float64
	load15     float64
	memBase    float64 // share of memory used by guests on a hypervisor
	memWalk    *walk
	cachedFrac float64

	swapTotal, swapUsed float64
	pageIn, pageOut     counter // pgpgin/pgpgout (minor operations)
	swapIn, swapOut     counter // pswpin/pswpout (major operations)
	faultMinor          counter
	faultMajor          counter

	fs    []*fsState
	disks []*diskState
	nics  []*nicState

	established *walk
	created     counter
	sleeping    int
	idleProcs   int

	procs   []*procState
	nextPID int
}

type fsState struct {
	device, mountpoint, fsType string
	size                       float64 // bytes
	reserved                   float64 // share kept for root (ext4)
	used                       *walk   // share used
	inodes                     float64
}

type diskState struct {
	name                  string
	readRate, writeRate   float64 // bytes per second when busy
	readLat, writeLat     float64 // seconds per operation
	act                   *walk
	readB, writeB         counter
	readOps, writeOps     float64
	readTime, writeTime   float64
	ioTime, weighted      float64
	mergedR, mergedW      float64
	pending               int
	lastReadB, lastWriteB float64
}

type nicState struct {
	name           string
	rate           float64 // bytes per second
	act            *walk
	rxB, txB       counter
	rxP, txP       float64
	rxErr, txErr   counter
	rxDrop, txDrop counter
	lastRx, lastTx float64
}

func newSystem(r *rand.Rand, isVM bool, vcpus int, memMB int, uptime time.Duration) *sysState {
	if vcpus <= 0 {
		vcpus = 1
	}
	if memMB <= 0 {
		memMB = 1024
	}
	s := &sysState{
		r:        r,
		isVM:     isVM,
		vcpus:    vcpus,
		physCPUs: vcpus,
		memTotal: float64(memMB) * mib,
		load:     newWalk(r, 0.02, 0.9, 0.02, 0.02),
		nextPID:  1000 + r.Intn(3000),
	}
	up := uptime.Seconds()
	s.cpuBias = make([]float64, vcpus)
	s.cpuTime = make([][]float64, vcpus)
	for i := range s.cpuBias {
		s.cpuBias[i] = 0.7 + 0.6*r.Float64()
		s.cpuTime[i] = make([]float64, len(cpuStates))
		s.advanceCPU(i, 0.25*s.cpuBias[i], up)
	}
	s.load1 = s.load.v * float64(vcpus)
	s.load5, s.load15 = s.load1, s.load1
	s.cachedFrac = 0.05 + 0.2*r.Float64()
	s.created.v = up * (1 + 4*r.Float64())
	s.faultMinor.v = up * (200 + 800*r.Float64())
	s.faultMajor.v = up * 0.05 * r.Float64()
	s.pageIn.v = up * (50 + 200*r.Float64())
	s.pageOut.v = up * (100 + 400*r.Float64())
	if isVM {
		s.established = newWalk(r, 2, 40, 1, 0.02)
		s.sleeping, s.idleProcs = 120+r.Intn(120), 50+r.Intn(40)
	} else {
		s.established = newWalk(r, 40, 400, 5, 0.02)
		s.sleeping, s.idleProcs = 900+r.Intn(900), 300+r.Intn(300)
	}
	return s
}

// newHostSystem is a hypervisor: SMT doubles the logical CPUs, guests hold most of the memory
// and the instance store fills with their disks.
func newHostSystem(r *rand.Rand, h *topology.Hypervisor, uptime time.Duration) *sysState {
	s := newSystem(r, false, h.VCPUs, h.MemoryMB, uptime)
	s.physCPUs = max(1, s.vcpus/2)
	guestMem, guestDisk := 0, 0
	for _, g := range h.GPUs {
		for _, vm := range g.VMs {
			guestMem += vm.MemoryMB
			guestDisk += vm.DiskGB
		}
	}
	s.memBase = clamp(float64(guestMem)*mib/s.memTotal+0.03, 0.05, 0.95)
	s.memWalk = newWalk(r, 0, 0.04, 0.003, 0.02)
	s.swapTotal = 8 * gib
	s.swapUsed = s.swapTotal * 0.02 * r.Float64()
	s.swapIn.v = uptime.Seconds() * 0.01 * r.Float64()
	s.swapOut.v = uptime.Seconds() * 0.02 * r.Float64()

	local := h.LocalGB
	if local <= 0 {
		local = 1000
	}
	instUsed := clamp(float64(guestDisk)/float64(local), 0.02, 0.95)
	s.fs = []*fsState{
		newFS(r, "/dev/mapper/ubuntu--vg-ubuntu--lv", "/", "ext4", 200*gib, 0.05, 0.15, 0.45),
		newFS(r, "/dev/nvme0n1p2", "/boot", "ext4", 2*gib, 0.05, 0.1, 0.2),
		newFS(r, "/dev/nvme0n1p1", "/boot/efi", "vfat", 1*gib, 0, 0.005, 0.01),
		newFS(r, "/dev/nvme1n1", "/var/lib/nova/instances", "xfs", float64(local)*gib, 0, instUsed, instUsed+0.02),
	}
	s.disks = []*diskState{
		newDisk(r, "dm-0", 2*mib, 6*mib, 0.0002, 0.0004),
		newDisk(r, "nvme0n1", 2*mib, 6*mib, 0.0002, 0.0004),
		newDisk(r, "nvme1n1", 40*mib, 80*mib, 0.0002, 0.0003),
	}
	s.nics = []*nicState{
		newNIC(r, "eno1", 2*mib),
		newNIC(r, "eno2", 60*mib),
		newNIC(r, "lo", 200*kib),
	}
	for _, g := range h.GPUs {
		for _, vm := range g.VMs {
			s.nics = append(s.nics, newNIC(r, tapName(vm.ID), 4*mib))
		}
	}
	for _, d := range s.disks {
		d.init(r, uptime.Seconds())
	}
	for _, n := range s.nics {
		n.init(r, uptime.Seconds())
	}
	return s
}

// newVMSystem is a cloud image guest: one virtio disk with an EFI partition, one NIC, no swap.
func newVMSystem(r *rand.Rand, vm *topology.VM, uptime time.Duration) *sysState {
	s := newSystem(r, true, vm.VCPUs, vm.MemoryMB, uptime)
	s.memWalk = newWalk(r, 0.08, 0.85, 0.01, 0.01)
	disk := vm.DiskGB
	if disk <= 0 {
		disk = 10
	}
	s.fs = []*fsState{
		newFS(r, "/dev/vda1", "/", "ext4", float64(disk)*gib-106*mib, 0.05, 0.1, 0.6),
		newFS(r, "/dev/vda15", "/boot/efi", "vfat", 105*mib, 0, 0.06, 0.06),
	}
	s.disks = []*diskState{
		newDisk(r, "vda", 8*mib, 20*mib, 0.0006, 0.0012),
		newDisk(r, "vda1", 8*mib, 20*mib, 0.0006, 0.0012),
		newDisk(r, "vda15", 1*kib, 1*kib, 0.0006, 0.0012),
	}
	s.nics = []*nicState{
		newNIC(r, "ens3", 4*mib),
		newNIC(r, "lo", 50*kib),
	}
	for _, d := range s.disks {
		d.init(r, uptime.Seconds())
	}
	for _, n := range s.nics {
		n.init(r, uptime.Seconds())
	}
	s.addVMProcesses(r, vm)
	return s
}

// tapName is the host interface nova creates for a VM's port: "tap" and the first 11
// characters of the port id. The port id is derived the same way the fake neutron derives it.
func tapName(vmID string) string {
	sum := sha1.Sum([]byte("give-me-real-gpu/openstack\x00port\x00" + vmID))
	return "tap" + fmt.Sprintf("%x-%x", sum[0:4], sum[4:6])[:11]
}

func newFS(r *rand.Rand, device, mount, typ string, size, reserved, lo, hi float64) *fsState {
	f := &fsState{device: device, mountpoint: mount, fsType: typ, size: size, reserved: reserved}
	f.used = newWalk(r, lo, math.Max(lo, hi), 0.0005, 0.002)
	f.used.idle = 0
	f.used.target = lo + (hi-lo)*r.Float64()
	f.used.v = f.used.target
	switch typ {
	case "ext4":
		f.inodes = math.Floor(size / (16 * kib))
	case "xfs":
		f.inodes = math.Floor(size / (64 * kib))
	}
	return f
}

func newDisk(r *rand.Rand, name string, readRate, writeRate, readLat, writeLat float64) *diskState {
	return &diskState{
		name: name, readRate: readRate, writeRate: writeRate, readLat: readLat, writeLat: writeLat,
		act: newWalk(r, 0, 1, 0.05, 0.03),
	}
}

func newNIC(r *rand.Rand, name string, rate float64) *nicState {
	return &nicState{name: name, rate: rate, act: newWalk(r, 0, 1, 0.05, 0.03)}
}

// advanceCPU adds dt seconds of CPU i at the busy share u to its state times.
func (s *sysState) advanceCPU(i int, u, dt float64) {
	u = clamp(u, 0, 1) * 0.97
	t := s.cpuTime[i]
	steal := 0.0
	if s.isVM {
		steal = 0.02
	}
	wait := 0.003 + 0.01*s.r.Float64()
	parts := []float64{0.72, 0.2 - steal, 0, 0.01, 0.01, 0.03, steal, 0}
	busy := 0.0
	for k, p := range parts {
		v := dt * u * p
		t[k] += v
		busy += v
	}
	t[7] += dt * wait
	busy += dt * wait
	t[2] += math.Max(0, dt-busy)
}

func (d *diskState) init(r *rand.Rand, up float64) {
	d.step(r, up*0.3, 1)
}

// step advances the disk by dt seconds at the busy share act.
func (d *diskState) step(r *rand.Rand, dt, act float64) {
	if dt <= 0 {
		return
	}
	rb := d.readB.add(r, d.readRate*act, dt)
	wb := d.writeB.add(r, d.writeRate*act, dt)
	dr, dw := rb-d.lastReadB, wb-d.lastWriteB
	d.lastReadB, d.lastWriteB = rb, wb
	ro, wo := dr/(64*kib), dw/(32*kib)
	d.readOps += ro
	d.writeOps += wo
	rt, wt := ro*d.readLat, wo*d.writeLat
	d.readTime += rt
	d.writeTime += wt
	d.ioTime += math.Min(dt, (rt+wt)*0.8)
	d.weighted += rt + wt
	d.mergedR += ro * 0.05
	d.mergedW += wo * 0.2
	d.pending = 0
	if act > 0.6 {
		d.pending = r.Intn(4)
	}
}

func (n *nicState) init(r *rand.Rand, up float64) {
	n.step(r, up*0.3, 1)
}

func (n *nicState) step(r *rand.Rand, dt, act float64) {
	if dt <= 0 {
		return
	}
	rx := n.rxB.add(r, n.rate*act, dt)
	tx := n.txB.add(r, n.rate*act*0.6, dt)
	n.rxP += (rx - n.lastRx) / 900
	n.txP += (tx - n.lastTx) / 700
	n.lastRx, n.lastTx = rx, tx
}

// emit writes the hostmetrics scrapers of the node.
func (s *sysState) emit(rm *otlp.ResourceMetrics, e *emitter, uptime time.Duration, dt float64) {
	r := s.r
	load := s.load.next(r)

	cpu := newScope(rm, scraperScope("cpu"))
	cpuTime := cpu.sum("system.cpu.time", "s", true)
	for i := range s.cpuTime {
		s.advanceCPU(i, load*s.cpuBias[i]*(0.8+0.4*r.Float64()), dt)
		name := otlp.String("cpu", "cpu"+strconv.Itoa(i))
		for k, st := range cpuStates {
			e.double(cpuTime, round(s.cpuTime[i][k], 2), name, otlp.String("state", st))
		}
	}
	e.int(cpu.sum("system.cpu.logical.count", "{cpu}", false), int64(s.vcpus))
	e.int(cpu.sum("system.cpu.physical.count", "{cpu}", false), int64(s.physCPUs))

	s.load1 = load * float64(s.vcpus) * (0.9 + 0.2*r.Float64())
	if dt > 0 {
		s.load5 += (s.load1 - s.load5) * (1 - math.Exp(-dt/300))
		s.load15 += (s.load1 - s.load15) * (1 - math.Exp(-dt/900))
	}
	ld := newScope(rm, scraperScope("load"))
	e.double(ld.gauge("system.cpu.load_average.1m", "{thread}"), round(s.load1, 2))
	e.double(ld.gauge("system.cpu.load_average.5m", "{thread}"), round(s.load5, 2))
	e.double(ld.gauge("system.cpu.load_average.15m", "{thread}"), round(s.load15, 2))

	s.emitMemory(rm, e)
	s.emitPaging(rm, e, dt)
	s.emitDisks(rm, e, dt)
	s.emitFilesystems(rm, e)
	s.emitNetwork(rm, e, dt)

	ps := newScope(rm, scraperScope("processes"))
	count := ps.sum("system.processes.count", "{processes}", false)
	running := 1 + int(load*float64(s.vcpus)*0.5) + r.Intn(3)
	blocked := 0
	if r.Float64() < 0.2 {
		blocked = 1 + r.Intn(2)
	}
	for _, st := range []struct {
		status string
		n      int
	}{{"blocked", blocked}, {"idle", s.idleProcs}, {"running", running}, {"sleeping", s.sleeping + r.Intn(20)}} {
		if st.n > 0 {
			e.int(count, int64(st.n), otlp.String("status", st.status))
		}
	}
	rate := 2.0
	if !s.isVM {
		rate = 15
	}
	e.int(ps.sum("system.processes.created", "{processes}", true), int64(s.created.add(r, rate, dt)))

	sys := newScope(rm, scraperScope("system"))
	e.double(sys.gauge("system.uptime", "s"), math.Floor(uptime.Seconds()))
}

func (s *sysState) emitMemory(rm *otlp.ResourceMetrics, e *emitter) {
	r := s.r
	used := s.memWalk.next(r)
	if !s.isVM {
		used = s.memBase + used
	}
	used = clamp(used, 0.02, 0.97)
	total := s.memTotal
	buffered := 0.01 * total
	slabR := 0.01 * total
	slabU := 0.004 * total
	usedB := used * total
	cached := math.Min(s.cachedFrac*total, math.Max(0, total-usedB-buffered-slabR)*0.8)
	free := math.Max(0, total-usedB-buffered-slabR-cached)
	inactive := cached*0.6 + usedB*0.1
	states := []struct {
		state string
		v     float64
	}{
		{"buffered", buffered}, {"cached", cached}, {"inactive", inactive}, {"free", free},
		{"slab_reclaimable", slabR}, {"slab_unreclaimable", slabU}, {"used", usedB},
	}
	sc := newScope(rm, scraperScope("memory"))
	usage := sc.sum("system.memory.usage", "By", false)
	util := sc.gauge("system.memory.utilization", "1")
	for _, st := range states {
		v := math.Floor(st.v)
		e.int(usage, int64(v), otlp.String("state", st.state))
		e.double(util, v/total, otlp.String("state", st.state))
	}
	e.int(sc.sum("system.memory.limit", "By", false), int64(total))
	e.int(sc.sum("system.linux.memory.available", "By", false), int64(free+cached+buffered+slabR))
	e.int(sc.sum("system.memory.linux.shared", "By", false), int64(total*0.002*(1+r.Float64())))
}

func (s *sysState) emitPaging(rm *otlp.ResourceMetrics, e *emitter, dt float64) {
	r := s.r
	sc := newScope(rm, scraperScope("paging"))
	if s.swapTotal > 0 {
		s.swapUsed = clamp(s.swapUsed+r.NormFloat64()*mib, 0, s.swapTotal*0.5)
		cached := math.Min(s.swapUsed*0.1, 64*mib)
		usage := sc.sum("system.paging.usage", "By", false)
		dev := otlp.String("device", "/swap.img")
		e.int(usage, int64(s.swapUsed), dev, otlp.String("state", "used"))
		e.int(usage, int64(s.swapTotal-s.swapUsed-cached), dev, otlp.String("state", "free"))
		e.int(usage, int64(cached), dev, otlp.String("state", "cached"))
	}
	ops := sc.sum("system.paging.operations", "{operations}", true)
	swapRate := 0.0
	if s.swapTotal > 0 {
		swapRate = 0.02
	}
	e.int(ops, int64(s.swapIn.add(r, swapRate, dt)), otlp.String("direction", "page_in"), otlp.String("type", "major"))
	e.int(ops, int64(s.swapOut.add(r, swapRate, dt)), otlp.String("direction", "page_out"), otlp.String("type", "major"))
	e.int(ops, int64(s.pageIn.add(r, 150, dt)), otlp.String("direction", "page_in"), otlp.String("type", "minor"))
	e.int(ops, int64(s.pageOut.add(r, 300, dt)), otlp.String("direction", "page_out"), otlp.String("type", "minor"))
	faults := sc.sum("system.paging.faults", "{faults}", true)
	e.int(faults, int64(s.faultMajor.add(r, 0.02, dt)), otlp.String("type", "major"))
	e.int(faults, int64(s.faultMinor.add(r, 500, dt)), otlp.String("type", "minor"))
}

func (s *sysState) emitDisks(rm *otlp.ResourceMetrics, e *emitter, dt float64) {
	sc := newScope(rm, scraperScope("disk"))
	io := sc.sum("system.disk.io", "By", true)
	ops := sc.sum("system.disk.operations", "{operations}", true)
	ioTime := sc.sum("system.disk.io_time", "s", true)
	opTime := sc.sum("system.disk.operation_time", "s", true)
	weighted := sc.sum("system.disk.weighted_io_time", "s", true)
	pending := sc.sum("system.disk.pending_operations", "{operations}", false)
	merged := sc.sum("system.disk.merged", "{operations}", true)
	read, write := otlp.String("direction", "read"), otlp.String("direction", "write")
	for _, d := range s.disks {
		d.step(s.r, dt, d.act.next(s.r))
		dev := otlp.String("device", d.name)
		e.int(io, int64(d.readB.v), dev, read)
		e.int(io, int64(d.writeB.v), dev, write)
		e.int(ops, int64(d.readOps), dev, read)
		e.int(ops, int64(d.writeOps), dev, write)
		e.double(ioTime, round(d.ioTime, 3), dev)
		e.double(opTime, round(d.readTime, 3), dev, read)
		e.double(opTime, round(d.writeTime, 3), dev, write)
		e.double(weighted, round(d.weighted, 3), dev)
		e.int(pending, int64(d.pending), dev)
		e.int(merged, int64(d.mergedR), dev, read)
		e.int(merged, int64(d.mergedW), dev, write)
	}
}

func (s *sysState) emitFilesystems(rm *otlp.ResourceMetrics, e *emitter) {
	sc := newScope(rm, scraperScope("filesystem"))
	usage := sc.sum("system.filesystem.usage", "By", false)
	inodes := sc.sum("system.filesystem.inodes.usage", "{inodes}", false)
	for _, f := range s.fs {
		used := math.Floor(f.used.next(s.r) * f.size)
		reserved := math.Floor(f.reserved * f.size)
		free := math.Max(0, f.size-used-reserved)
		attrs := func(state string) []otlp.KeyValue {
			return []otlp.KeyValue{
				otlp.String("device", f.device),
				otlp.String("mode", "rw"),
				otlp.String("mountpoint", f.mountpoint),
				otlp.String("type", f.fsType),
				otlp.String("state", state),
			}
		}
		e.int(usage, int64(free), attrs("free")...)
		e.int(usage, int64(reserved), attrs("reserved")...)
		e.int(usage, int64(used), attrs("used")...)
		iu := math.Floor(f.inodes * math.Min(1, used/f.size*0.3))
		e.int(inodes, int64(f.inodes-iu), attrs("free")...)
		e.int(inodes, int64(iu), attrs("used")...)
	}
}

func (s *sysState) emitNetwork(rm *otlp.ResourceMetrics, e *emitter, dt float64) {
	r := s.r
	sc := newScope(rm, scraperScope("network"))
	io := sc.sum("system.network.io", "By", true)
	packets := sc.sum("system.network.packets", "{packets}", true)
	errs := sc.sum("system.network.errors", "{errors}", true)
	dropped := sc.sum("system.network.dropped", "{packets}", true)
	rx, tx := otlp.String("direction", "receive"), otlp.String("direction", "transmit")
	for _, n := range s.nics {
		n.step(r, dt, n.act.next(r))
		dev := otlp.String("device", n.name)
		e.int(io, int64(n.rxB.v), dev, rx)
		e.int(io, int64(n.txB.v), dev, tx)
		e.int(packets, int64(n.rxP), dev, rx)
		e.int(packets, int64(n.txP), dev, tx)
		pDrop := 0.0
		if n.name != "lo" {
			pDrop = 0.01
		}
		e.int(errs, int64(n.rxErr.bump(r, pDrop/10)), dev, rx)
		e.int(errs, int64(n.txErr.bump(r, 0)), dev, tx)
		e.int(dropped, int64(n.rxDrop.bump(r, pDrop)), dev, rx)
		e.int(dropped, int64(n.txDrop.bump(r, 0)), dev, tx)
	}
	conns := sc.sum("system.network.connections", "{connections}", false)
	est := int64(s.established.next(r))
	listen := int64(8)
	if !s.isVM {
		listen = 40
	}
	tcp := otlp.String("protocol", "tcp")
	for _, st := range tcpStates {
		var n int64
		switch st {
		case "ESTABLISHED":
			n = est
		case "LISTEN":
			n = listen
		case "TIME_WAIT":
			n = int64(r.Intn(int(est/2) + 1))
		case "CLOSE_WAIT", "FIN_WAIT_2":
			n = int64(r.Intn(2))
		}
		e.int(conns, n, tcp, otlp.String("state", st))
	}
}

// procState is one process the process scraper reads.
type procState struct {
	name, path, cmdline, owner string
	pid, ppid                  int
	startAfter                 time.Duration // after boot
	cores                      *walk         // CPUs in use
	mem                        *walk         // resident bytes
	rss, virt                  float64
	user, system, wait         float64
	readB, writeB              counter
	ioRate                     float64
	cpuNow                     float64
}

// addProc adds a process using up to coresHi CPUs and rssLo to rssHi bytes of resident memory.
func (s *sysState) addProc(r *rand.Rand, name, path, cmdline, owner string, ppid int, coresHi, rssLo, rssHi, ioRate float64, start time.Duration) {
	pid := 1
	if name != "systemd" {
		s.nextPID += 1 + r.Intn(400)
		pid = s.nextPID
	}
	p := &procState{
		name: name, path: path, cmdline: cmdline, owner: owner, pid: pid, ppid: ppid,
		startAfter: start, cores: newWalk(r, 0, coresHi, coresHi*0.03, 0.02),
		mem:  newWalk(r, rssLo, math.Max(rssLo, rssHi), (rssHi-rssLo)*0.01, 0.01),
		virt: rssHi*1.6 + 300*mib, ioRate: ioRate,
	}
	p.mem.idle = 0
	p.rss = p.mem.v
	s.procs = append(s.procs, p)
}

// addHostProcesses lists the hypervisor's processes: one qemu (and on vGPU and MIG nodes one
// nvidia-vgpu-mgr) per VM next to the compute daemons.
func (s *sysState) addHostProcesses(r *rand.Rand, h *topology.Hypervisor, vms []*vmGPU) {
	boot := 5 * time.Second
	s.addProc(r, "systemd", "/usr/lib/systemd/systemd", "/sbin/init", "root", 0, 0.05, 12*mib, 14*mib, 2*kib, 0)
	s.addProc(r, "libvirtd", "/usr/sbin/libvirtd", "/usr/sbin/libvirtd", "root", 1, 0.3, 50*mib, 70*mib, 10*kib, boot)
	s.addProc(r, "ovs-vswitchd", "/usr/sbin/ovs-vswitchd", "ovs-vswitchd unix:/var/run/openvswitch/db.sock -vconsole:emer -vsyslog:err -vfile:info --mlockall --no-chdir --pidfile --detach",
		"root", 1, 1.2, 240*mib, 260*mib, 4*kib, boot)
	s.addProc(r, "nova-compute", "/usr/bin/python3.12", "/usr/bin/python3 /usr/bin/nova-compute --config-file=/etc/nova/nova.conf --log-file=/var/log/nova/nova-compute.log",
		"nova", 1, 0.4, 160*mib, 220*mib, 20*kib, boot+20*time.Second)
	if h.DriverVersion != "" {
		s.addProc(r, "nvidia-persistenced", "/usr/bin/nvidia-persistenced", "/usr/bin/nvidia-persistenced --user nvidia-persistenced --persistence-mode --verbose",
			"nvidia-persistenced", 1, 0.01, 7*mib, 8*mib, 0, boot)
	}
	for i, vg := range vms {
		vm := vg.vm
		start := time.Duration(float64(time.Hour) * (1 + 24*r.Float64()))
		cmd := "/usr/bin/qemu-system-x86_64 -name guest=" + vm.InstanceName + ",debug-threads=on -S -uuid " + vm.ID +
			" -smp " + strconv.Itoa(max(1, vm.VCPUs)) + ",sockets=" + strconv.Itoa(max(1, vm.VCPUs)) + ",cores=1,threads=1 -m size=" +
			strconv.Itoa(vm.MemoryMB*1024) + "k"
		vmMem := float64(max(1024, vm.MemoryMB)) * mib
		s.addProc(r, "qemu-system-x86_64", "/usr/bin/qemu-system-x86_64", cmd, "libvirt-qemu", 1,
			float64(max(1, vm.VCPUs))*0.9, vmMem*0.4, vmMem*1.02, 2*mib, start)
		if h.Mode == topology.ModeVGPU || h.Mode == topology.ModeMIG {
			s.addProc(r, "nvidia-vgpu-mgr", "/usr/bin/nvidia-vgpu-mgr", "/usr/bin/nvidia-vgpu-mgr", "root", 1, 0.08, 35*mib, 45*mib, 0,
				start+time.Duration(i)*time.Millisecond)
		}
	}
}

// addVMProcesses lists a GPU guest's processes: the GPU workload, the collection agent and the usual daemons.
func (s *sysState) addVMProcesses(r *rand.Rand, vm *topology.VM) {
	boot := 3 * time.Second
	vcpus := float64(s.vcpus)
	s.addProc(r, "systemd", "/usr/lib/systemd/systemd", "/sbin/init", "root", 0, 0.02, 11*mib, 13*mib, 1*kib, 0)
	s.addProc(r, "systemd-journal", "/usr/lib/systemd/systemd-journald", "/usr/lib/systemd/systemd-journald", "root", 1, 0.02, 20*mib, 40*mib, 8*kib, boot)
	s.addProc(r, "sshd", "/usr/sbin/sshd", "sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups", "root", 1, 0.01, 7*mib, 9*mib, 0, boot)
	s.addProc(r, "cron", "/usr/sbin/cron", "/usr/sbin/cron -f -P", "root", 1, 0.005, 3*mib, 3*mib, 0, boot)
	s.addProc(r, "snapd", "/usr/lib/snapd/snapd", "/usr/lib/snapd/snapd", "root", 1, 0.02, 25*mib, 35*mib, 1*kib, boot)
	s.addProc(r, "otelcol", "/usr/local/bin/otelcol", "/usr/local/bin/otelcol --config=/etc/otelcol/config.yaml", "root", 1, 0.08, 100*mib, 140*mib, 4*kib, boot+5*time.Second)
	if vm.GuestDriverVersion != "" {
		s.addProc(r, "nvidia-persistenced", "/usr/bin/nvidia-persistenced", "/usr/bin/nvidia-persistenced --user nvidia-persistenced --persistence-mode --verbose",
			"nvidia-persistenced", 1, 0.01, 7*mib, 8*mib, 0, boot)
	}
	start := time.Duration(float64(time.Hour) * (1 + 6*r.Float64()))
	s.addProc(r, "python3", "/usr/bin/python3.12", "python3 train.py --config configs/run.yaml", "ubuntu", 1,
		math.Max(1, vcpus*0.6), s.memTotal*0.05, s.memTotal*0.45, 5*mib, start)
	s.addProc(r, "python3", "/usr/bin/python3.12", "python3 -m jupyterlab --no-browser --port=8888", "ubuntu", 1, 0.1, 120*mib, 200*mib, 10*kib, start-30*time.Minute)
}

// topProcesses is how many processes each of CPU and memory keeps.
const topProcesses = 10

// emitProcesses writes the process scraper's metrics of the processes using the most CPU and
// the most memory, one resource per process as the scraper sends them. The process identity
// is on the resource and, as the top process processor copies it, on every point.
func (s *sysState) emitProcesses(e *emitter, boot time.Time, dt float64) []*otlp.ResourceMetrics {
	r := s.r
	vcpus := float64(s.vcpus)
	var live []*procState
	for _, p := range s.procs {
		if boot.Add(p.startAfter).After(time.Unix(0, int64(e.ts))) {
			continue
		}
		p.cpuNow = p.cores.next(r)
		p.rss = math.Min(p.mem.next(r), s.memTotal*0.98)
		if dt > 0 {
			p.user += p.cpuNow * dt * 0.8
			p.system += p.cpuNow * dt * 0.18
			p.wait += p.cpuNow * dt * 0.02
		}
		p.readB.add(r, p.ioRate*0.3, dt)
		p.writeB.add(r, p.ioRate, dt)
		live = append(live, p)
	}
	keep := map[*procState]bool{}
	for _, by := range []func(p *procState) float64{
		func(p *procState) float64 { return p.cpuNow },
		func(p *procState) float64 { return p.rss },
	} {
		ranked := append([]*procState(nil), live...)
		sortProcs(ranked, by)
		for i := 0; i < len(ranked) && i < topProcesses; i++ {
			keep[ranked[i]] = true
		}
	}
	var out []*otlp.ResourceMetrics
	for _, p := range live {
		if !keep[p] {
			continue
		}
		rm := &otlp.ResourceMetrics{Resource: []otlp.KeyValue{
			otlp.Int("process.pid", int64(p.pid)),
			otlp.Int("process.parent_pid", int64(p.ppid)),
			otlp.String("process.executable.name", p.name),
			otlp.String("process.executable.path", p.path),
			otlp.String("process.command", strings.Fields(p.cmdline)[0]),
			otlp.String("process.command_line", p.cmdline),
			otlp.String("process.owner", p.owner),
		}}
		pe := e.withStart(boot.Add(p.startAfter))
		id := []otlp.KeyValue{otlp.String("process.executable.name", p.name), otlp.Int("process.pid", int64(p.pid))}
		with := func(extra ...otlp.KeyValue) []otlp.KeyValue {
			return append(append([]otlp.KeyValue(nil), extra...), id...)
		}
		sc := newScope(rm, scraperScope("process"))
		cpuTime := sc.sum("process.cpu.time", "s", true)
		pe.double(cpuTime, round(p.user, 2), with(otlp.String("state", "user"))...)
		pe.double(cpuTime, round(p.system, 2), with(otlp.String("state", "system"))...)
		pe.double(cpuTime, round(p.wait, 2), with(otlp.String("state", "wait"))...)
		util := sc.gauge("process.cpu.utilization", "1")
		pe.double(util, p.cpuNow*0.8/vcpus, with(otlp.String("state", "user"))...)
		pe.double(util, p.cpuNow*0.18/vcpus, with(otlp.String("state", "system"))...)
		pe.double(util, p.cpuNow*0.02/vcpus, with(otlp.String("state", "wait"))...)
		pe.int(sc.sum("process.memory.usage", "By", false), int64(p.rss), with()...)
		pe.int(sc.sum("process.memory.virtual", "By", false), int64(math.Max(p.virt, p.rss)), with()...)
		// hostmetrics reports a percentage here under unit 1 (gopsutil MemoryPercent).
		pe.double(sc.gauge("process.memory.utilization", "1"), round(p.rss/s.memTotal*100, 4), with()...)
		dio := sc.sum("process.disk.io", "By", true)
		pe.int(dio, int64(p.readB.v), with(otlp.String("direction", "read"))...)
		pe.int(dio, int64(p.writeB.v), with(otlp.String("direction", "write"))...)
		out = append(out, rm)
	}
	return out
}

// sortProcs orders processes by key, largest first, keeping the list order on ties.
func sortProcs(ps []*procState, key func(*procState) float64) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && key(ps[j]) > key(ps[j-1]); j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}
