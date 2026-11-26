package openstack

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

const (
	novaMinMinor = 1
	novaMaxMinor = 103
)

// novaMicroversion returns the minor part of the requested compute
// microversion (2.<minor>). Without a header nova answers as 2.1.
func novaMicroversion(r *http.Request) int {
	v := ""
	for _, h := range r.Header.Values("OpenStack-API-Version") {
		for _, part := range strings.Split(h, ",") {
			f := strings.Fields(part)
			if len(f) == 2 && strings.EqualFold(f[0], "compute") {
				v = f[1]
			}
		}
	}
	if v == "" {
		v = strings.TrimSpace(r.Header.Get("X-OpenStack-Nova-API-Version"))
	}
	if v == "" {
		return novaMinMinor
	}
	if strings.EqualFold(v, "latest") {
		return novaMaxMinor
	}
	major, minor, ok := strings.Cut(v, ".")
	if !ok || major != "2" {
		return novaMinMinor
	}
	n, err := strconv.Atoi(minor)
	if err != nil || n < novaMinMinor {
		return novaMinMinor
	}
	return min(n, novaMaxMinor)
}

// nova wraps a compute handler so that every answer carries the
// microversion headers nova sends.
func nova(h func(w http.ResponseWriter, r *http.Request, mv int)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mv := novaMicroversion(r)
		v := "2." + itoa(mv)
		w.Header().Set("OpenStack-API-Version", "compute "+v)
		w.Header().Set("X-OpenStack-Nova-API-Version", v)
		w.Header().Set("Vary", "OpenStack-API-Version, X-OpenStack-Nova-API-Version")
		h(w, r, mv)
	}
}

func (s *server) novaRoutes() {
	m := s.mux
	const p = "/compute/v2.1"
	m.HandleFunc("GET /compute", s.novaVersions)
	m.HandleFunc("GET /compute/{$}", s.novaVersions)
	m.HandleFunc("GET "+p, s.novaVersion)
	m.HandleFunc("GET "+p+"/{$}", s.novaVersion)
	m.HandleFunc("GET /compute/v2", s.novaVersion)
	m.HandleFunc("GET /compute/v2/{$}", s.novaVersion)

	m.HandleFunc("GET "+p+"/os-hypervisors", nova(s.listHypervisors))
	m.HandleFunc("GET "+p+"/os-hypervisors/detail", nova(s.listHypervisorsDetail))
	m.HandleFunc("GET "+p+"/os-hypervisors/statistics", nova(s.hypervisorStatistics))
	m.HandleFunc("GET "+p+"/os-hypervisors/{id}", nova(s.getHypervisor))
	m.HandleFunc("GET "+p+"/os-hypervisors/{host}/servers", nova(s.hypervisorServers))

	m.HandleFunc("GET "+p+"/servers", nova(s.listServers))
	m.HandleFunc("GET "+p+"/servers/detail", nova(s.listServersDetail))
	m.HandleFunc("GET "+p+"/servers/{id}", nova(s.getServer))
	m.HandleFunc("GET "+p+"/servers/{id}/diagnostics", nova(s.serverDiagnostics))
	m.HandleFunc("GET "+p+"/servers/{id}/os-interface", nova(s.serverInterfaces))
	m.HandleFunc("GET "+p+"/servers/{id}/os-instance-actions", nova(s.serverActions))
	m.HandleFunc("GET "+p+"/servers/{id}/metadata", nova(s.serverMetadata))

	m.HandleFunc("GET "+p+"/flavors", nova(s.listFlavors))
	m.HandleFunc("GET "+p+"/flavors/detail", nova(s.listFlavorsDetail))
	m.HandleFunc("GET "+p+"/flavors/{id}", nova(s.getFlavor))
	m.HandleFunc("GET "+p+"/flavors/{id}/os-extra_specs", nova(s.flavorExtraSpecs))
	m.HandleFunc("GET "+p+"/flavors/{id}/os-extra_specs/{key}", nova(s.flavorExtraSpec))
	m.HandleFunc("GET "+p+"/flavors/{id}/os-flavor-access", nova(s.flavorAccess))

	m.HandleFunc("GET "+p+"/os-services", nova(s.computeServices))
	m.HandleFunc("GET "+p+"/os-availability-zone", nova(s.availabilityZones))
	m.HandleFunc("GET "+p+"/os-availability-zone/detail", nova(s.availabilityZonesDetail))
	m.HandleFunc("GET "+p+"/limits", nova(s.computeLimits))
	m.HandleFunc("GET "+p+"/os-quota-sets/{project}", nova(s.computeQuota))
	m.HandleFunc("GET "+p+"/os-quota-sets/{project}/defaults", nova(s.computeQuota))
	m.HandleFunc("GET "+p+"/os-quota-sets/{project}/detail", nova(s.computeQuotaDetail))
	for path, key := range map[string]string{
		"/os-keypairs":                 "keypairs",
		"/os-aggregates":               "aggregates",
		"/os-server-groups":            "server_groups",
		"/os-simple-tenant-usage":      "tenant_usages",
		"/os-migrations":               "migrations",
		"/os-instance_usage_audit_log": "instance_usage_audit_logs",
	} {
		m.HandleFunc("GET "+p+path, nova(emptyList(key)))
	}
	m.HandleFunc("GET "+p+"/images/{id}", nova(func(w http.ResponseWriter, r *http.Request, _ int) {
		writeError(w, http.StatusNotFound, "itemNotFound", "Image not found.")
	}))
}

func emptyList(key string) func(w http.ResponseWriter, r *http.Request, mv int) {
	return func(w http.ResponseWriter, r *http.Request, _ int) {
		writeJSON(w, http.StatusOK, obj{key: []obj{}})
	}
}

func (s *server) novaVersionDoc(r *http.Request, id string) obj {
	base := requestBase(r) + "/compute/"
	if id == "v2.0" {
		return obj{
			"id": "v2.0", "status": "SUPPORTED", "version": "", "min_version": "",
			"updated": "2011-01-21T11:33:21Z",
			"links":   []obj{{"rel": "self", "href": base + "v2/"}},
		}
	}
	return obj{
		"id": "v2.1", "status": "CURRENT",
		"version":     "2." + itoa(novaMaxMinor),
		"min_version": "2." + itoa(novaMinMinor),
		"updated":     "2013-07-23T11:33:21Z",
		"links": []obj{
			{"rel": "self", "href": base + "v2.1/"},
			{"rel": "describedby", "type": "text/html", "href": "https://docs.openstack.org/"},
		},
		"media-types": []obj{{"base": "application/json", "type": "application/vnd.openstack.compute+json;version=2.1"}},
	}
}

func (s *server) novaVersions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"versions": []obj{s.novaVersionDoc(r, "v2.0"), s.novaVersionDoc(r, "v2.1")}})
}

func (s *server) novaVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"version": s.novaVersionDoc(r, "v2.1")})
}

// hypervisorUsage sums what the VMs on h consume.
type hypervisorUsage struct {
	vcpus, memMB, diskGB, vms int
}

const reservedHostMemoryMB = 512

func usageOf(h *topology.Hypervisor) hypervisorUsage {
	u := hypervisorUsage{memMB: reservedHostMemoryMB}
	for _, g := range h.GPUs {
		for _, vm := range g.VMs {
			u.vcpus += vm.VCPUs
			u.memMB += vm.MemoryMB
			u.diskGB += vm.DiskGB
			u.vms++
		}
	}
	return u
}

// computeServiceID is the nova-compute service id of the i-th hypervisor.
// Ids 1 and 2 belong to nova-conductor and nova-scheduler.
func computeServiceID(i int) int { return i + 3 }

const controllerHost = "emu-controller"

func (s *server) hypervisorID(h *topology.Hypervisor, mv int) any {
	if mv >= 53 {
		return h.UUID
	}
	return h.ID
}

// cpuInfo is the libvirt CPU description. Below microversion 2.28 nova sends
// it as a JSON string, which is what openstack4j parses
// (ExtHypervisor.HypervisorCPUInfo has a @JsonCreator taking a String).
func cpuInfo(h *topology.Hypervisor) obj {
	threads, sockets := 2, 2
	cores := h.VCPUs / (threads * sockets)
	if cores < 1 {
		cores = 1
	}
	return obj{
		"arch":     "x86_64",
		"model":    "Cascadelake-Server",
		"vendor":   "Intel",
		"topology": obj{"cells": sockets, "sockets": sockets, "cores": cores, "threads": threads},
		"features": []string{"pge", "avx", "avx2", "avx512f", "sse4.2", "vmx", "aes", "x2apic"},
	}
}

func (s *server) hypervisorSummary(i int, h *topology.Hypervisor, mv int) obj {
	return obj{
		"id":                  s.hypervisorID(h, mv),
		"hypervisor_hostname": h.Hostname,
		"state":               "up",
		"status":              "enabled",
	}
}

func (s *server) hypervisorDetail(i int, h *topology.Hypervisor, mv int) obj {
	u := usageOf(h)
	out := s.hypervisorSummary(i, h, mv)
	var svcID any = computeServiceID(i)
	if mv >= 53 {
		svcID = stableUUID("nova-service", h.Hostname, "nova-compute")
	}
	out["hypervisor_type"] = "QEMU"
	out["hypervisor_version"] = 8002002
	out["host_ip"] = h.HostIP
	out["service"] = obj{"host": h.Hostname, "id": svcID, "disabled_reason": nil}
	ci := cpuInfo(h)
	if mv >= 28 {
		out["cpu_info"] = ci
	} else {
		b, _ := json.Marshal(ci)
		out["cpu_info"] = string(b)
	}
	if mv < 88 {
		freeDisk := h.LocalGB - u.diskGB
		out["vcpus"] = h.VCPUs
		out["vcpus_used"] = u.vcpus
		out["memory_mb"] = h.MemoryMB
		out["memory_mb_used"] = u.memMB
		out["local_gb"] = h.LocalGB
		out["local_gb_used"] = u.diskGB
		out["free_ram_mb"] = h.MemoryMB - u.memMB
		out["free_disk_gb"] = freeDisk
		out["disk_available_least"] = freeDisk - freeDisk/20
		out["current_workload"] = 0
		out["running_vms"] = u.vms
	} else {
		out["uptime"] = " 10:00:00 up 30 days,  1:00,  0 users,  load average: 0.50, 0.40, 0.30"
	}
	return out
}

func (s *server) listHypervisors(w http.ResponseWriter, r *http.Request, mv int) {
	var out []obj
	for i, h := range s.topo.Hypervisors {
		out = append(out, s.hypervisorSummary(i, h, mv))
	}
	writeJSON(w, http.StatusOK, obj{"hypervisors": list(out)})
}

func (s *server) listHypervisorsDetail(w http.ResponseWriter, r *http.Request, mv int) {
	var out []obj
	for i, h := range s.topo.Hypervisors {
		out = append(out, s.hypervisorDetail(i, h, mv))
	}
	writeJSON(w, http.StatusOK, obj{"hypervisors": list(out)})
}

// findHypervisor accepts the integer id, the uuid or the hostname, because
// clients mix them up (some pass the hostname as the hypervisor id).
func (s *server) findHypervisor(key string) (int, *topology.Hypervisor) {
	for i, h := range s.topo.Hypervisors {
		if itoa(h.ID) == key || h.UUID == key || h.Hostname == key {
			return i, h
		}
	}
	return -1, nil
}

func (s *server) getHypervisor(w http.ResponseWriter, r *http.Request, mv int) {
	i, h := s.findHypervisor(r.PathValue("id"))
	if h == nil {
		writeError(w, http.StatusNotFound, "itemNotFound", "Hypervisor with ID "+r.PathValue("id")+" could not be found.")
		return
	}
	writeJSON(w, http.StatusOK, obj{"hypervisor": s.hypervisorDetail(i, h, mv)})
}

func (s *server) hypervisorStatistics(w http.ResponseWriter, r *http.Request, mv int) {
	var t struct{ vcpus, vcpusUsed, mem, memUsed, disk, diskUsed, vms int }
	for _, h := range s.topo.Hypervisors {
		u := usageOf(h)
		t.vcpus += h.VCPUs
		t.vcpusUsed += u.vcpus
		t.mem += h.MemoryMB
		t.memUsed += u.memMB
		t.disk += h.LocalGB
		t.diskUsed += u.diskGB
		t.vms += u.vms
	}
	writeJSON(w, http.StatusOK, obj{"hypervisor_statistics": obj{
		"count":                len(s.topo.Hypervisors),
		"vcpus":                t.vcpus,
		"vcpus_used":           t.vcpusUsed,
		"memory_mb":            t.mem,
		"memory_mb_used":       t.memUsed,
		"free_ram_mb":          t.mem - t.memUsed,
		"local_gb":             t.disk,
		"local_gb_used":        t.diskUsed,
		"free_disk_gb":         t.disk - t.diskUsed,
		"disk_available_least": t.disk - t.diskUsed,
		"current_workload":     0,
		"running_vms":          t.vms,
	}})
}

func vmsOn(h *topology.Hypervisor) []*topology.VM {
	var out []*topology.VM
	for _, g := range h.GPUs {
		out = append(out, g.VMs...)
	}
	return out
}

// hypervisorServers answers GET /os-hypervisors/{pattern}/servers. Nova
// matches the pattern as a substring; the exact match is put first because
// clients read element 0. The servers key is left out for idle hosts, as
// nova does.
func (s *server) hypervisorServers(w http.ResponseWriter, r *http.Request, mv int) {
	pattern := r.PathValue("host")
	var exact, partial []obj
	for i, h := range s.topo.Hypervisors {
		if !strings.Contains(h.Hostname, pattern) {
			continue
		}
		e := s.hypervisorSummary(i, h, mv)
		var servers []obj
		for _, vm := range vmsOn(h) {
			servers = append(servers, obj{"uuid": vm.ID, "name": vm.InstanceName})
		}
		if len(servers) > 0 {
			e["servers"] = servers
		}
		if h.Hostname == pattern {
			exact = append(exact, e)
		} else {
			partial = append(partial, e)
		}
	}
	out := append(exact, partial...)
	if len(out) == 0 {
		writeError(w, http.StatusNotFound, "itemNotFound", "No hypervisor matching '"+pattern+"' could be found.")
		return
	}
	writeJSON(w, http.StatusOK, obj{"hypervisors": out})
}

func (s *server) findVM(id string) *topology.VM {
	for _, vm := range s.topo.AllVMs() {
		if vm.ID == id {
			return vm
		}
	}
	return nil
}

func (s *server) serverLinks(id string) []obj {
	base := s.baseURL() + "/compute"
	return []obj{
		{"rel": "self", "href": base + "/v2.1/servers/" + id},
		{"rel": "bookmark", "href": base + "/servers/" + id},
	}
}

func (s *server) flavorLinks(id string) []obj {
	base := s.baseURL() + "/compute"
	return []obj{
		{"rel": "self", "href": base + "/v2.1/flavors/" + id},
		{"rel": "bookmark", "href": base + "/flavors/" + id},
	}
}

// serverFlavor is the flavor reference inside a server. Before 2.47 it is
// {id, links}; openstack4j then fetches /flavors/{id} lazily when the name
// is needed (NovaServer.getFlavor). From 2.47 nova embeds the flavor body.
func (s *server) serverFlavor(vm *topology.VM, mv int) obj {
	f := s.topo.FlavorByID(vm.FlavorID)
	if mv < 47 || f == nil {
		return obj{"id": vm.FlavorID, "links": s.flavorLinks(vm.FlavorID)}
	}
	return obj{
		"original_name": f.Name,
		"vcpus":         f.VCPUs,
		"ram":           f.RAMMB,
		"disk":          f.DiskGB,
		"ephemeral":     0,
		"swap":          0,
		"extra_specs":   extraSpecs(f),
	}
}

func (s *server) serverAddresses(vm *topology.VM) obj {
	if vm.IP == "" {
		return obj{}
	}
	return obj{s.net.name: []obj{{
		"addr":                    vm.IP,
		"version":                 4,
		"OS-EXT-IPS:type":         "fixed",
		"OS-EXT-IPS-MAC:mac_addr": macFor(portID(vm.ID)),
	}}}
}

func (s *server) serverDetail(vm *topology.VM, mv int) obj {
	created := createdAt(vm.ID)
	out := obj{
		"id":                                   vm.ID,
		"name":                                 vm.Name,
		"status":                               "ACTIVE",
		"tenant_id":                            s.topo.ProjectID,
		"user_id":                              s.topo.UserID,
		"metadata":                             obj{},
		"hostId":                               hostIDFor(s.topo.ProjectID, vm.Hypervisor),
		"image":                                "",
		"flavor":                               s.serverFlavor(vm, mv),
		"created":                              fmtTime(created, layoutISOZ),
		"updated":                              fmtTime(created.Add(90e9), layoutISOZ),
		"addresses":                            s.serverAddresses(vm),
		"accessIPv4":                           "",
		"accessIPv6":                           "",
		"links":                                s.serverLinks(vm.ID),
		"OS-DCF:diskConfig":                    "MANUAL",
		"progress":                             0,
		"OS-EXT-AZ:availability_zone":          "nova",
		"config_drive":                         "",
		"key_name":                             nil,
		"OS-SRV-USG:launched_at":               fmtTime(created.Add(60e9), layoutMicro),
		"OS-SRV-USG:terminated_at":             nil,
		"OS-EXT-SRV-ATTR:host":                 vm.Hypervisor,
		"OS-EXT-SRV-ATTR:instance_name":        vm.InstanceName,
		"OS-EXT-SRV-ATTR:hypervisor_hostname":  vm.Hypervisor,
		"OS-EXT-STS:task_state":                nil,
		"OS-EXT-STS:vm_state":                  "active",
		"OS-EXT-STS:power_state":               1,
		"os-extended-volumes:volumes_attached": []obj{},
		"security_groups":                      []obj{{"name": "default"}},
	}
	if mv >= 3 {
		out["OS-EXT-SRV-ATTR:hostname"] = vm.Name
		out["OS-EXT-SRV-ATTR:reservation_id"] = "r-" + stableHex32("reservation", vm.ID)[:8]
		out["OS-EXT-SRV-ATTR:launch_index"] = 0
		out["OS-EXT-SRV-ATTR:kernel_id"] = ""
		out["OS-EXT-SRV-ATTR:ramdisk_id"] = ""
		out["OS-EXT-SRV-ATTR:root_device_name"] = "/dev/vda"
		out["OS-EXT-SRV-ATTR:user_data"] = nil
	}
	if mv >= 9 {
		out["locked"] = false
	}
	if mv >= 19 {
		out["description"] = nil
	}
	if mv >= 26 {
		out["tags"] = []string{}
	}
	return out
}

func (s *server) listServers(w http.ResponseWriter, r *http.Request, mv int) {
	var out []obj
	for _, vm := range s.topo.AllVMs() {
		out = append(out, obj{"id": vm.ID, "name": vm.Name, "links": s.serverLinks(vm.ID)})
	}
	writeJSON(w, http.StatusOK, obj{"servers": list(out)})
}

// listServersDetail ignores all_tenants and other filters: every VM belongs
// to the single project and the client asks for all of them.
func (s *server) listServersDetail(w http.ResponseWriter, r *http.Request, mv int) {
	var out []obj
	for _, vm := range s.topo.AllVMs() {
		out = append(out, s.serverDetail(vm, mv))
	}
	writeJSON(w, http.StatusOK, obj{"servers": list(out)})
}

func (s *server) serverNotFound(w http.ResponseWriter, id string) {
	writeError(w, http.StatusNotFound, "itemNotFound", "Instance "+id+" could not be found.")
}

func (s *server) getServer(w http.ResponseWriter, r *http.Request, mv int) {
	vm := s.findVM(r.PathValue("id"))
	if vm == nil {
		s.serverNotFound(w, r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, obj{"server": s.serverDetail(vm, mv)})
}

// serverDiagnostics uses the flat libvirt format of microversions below
// 2.48. openstack4j reads it as Map<String, Number>, so every value must be
// a number. cpu<N>_time and memory-rss are the keys clients read.
func (s *server) serverDiagnostics(w http.ResponseWriter, r *http.Request, mv int) {
	vm := s.findVM(r.PathValue("id"))
	if vm == nil {
		s.serverNotFound(w, r.PathValue("id"))
		return
	}
	now := s.now()
	up := now.Sub(createdAt(vm.ID).Add(60e9)).Seconds()
	if up < 1 {
		up = 1
	}
	memKiB := vm.MemoryMB * 1024
	rssPct := 55 + int(stableUint("rss", vm.ID)%30)
	out := obj{
		"memory":           memKiB,
		"memory-actual":    memKiB,
		"memory-available": memKiB - memKiB/50,
		"memory-unused":    memKiB * (100 - rssPct) / 100,
		"memory-rss":       memKiB * rssPct / 100,
		"vda_read":         int64(up * 40960),
		"vda_read_req":     int64(up * 10),
		"vda_write":        int64(up * 81920),
		"vda_write_req":    int64(up * 20),
		"vda_errors":       -1,
	}
	for c := 0; c < vm.VCPUs; c++ {
		out["cpu"+itoa(c)+"_time"] = int64(up * 1e9 * float64(20+c%7) / 100)
	}
	tap := "tap" + portID(vm.ID)[:11]
	out[tap+"_rx"] = int64(up * 2048)
	out[tap+"_rx_packets"] = int64(up * 12)
	out[tap+"_rx_errors"] = 0
	out[tap+"_rx_drop"] = 0
	out[tap+"_tx"] = int64(up * 1024)
	out[tap+"_tx_packets"] = int64(up * 8)
	out[tap+"_tx_errors"] = 0
	out[tap+"_tx_drop"] = 0
	writeJSON(w, http.StatusOK, out)
}

func (s *server) serverInterfaces(w http.ResponseWriter, r *http.Request, mv int) {
	vm := s.findVM(r.PathValue("id"))
	if vm == nil {
		s.serverNotFound(w, r.PathValue("id"))
		return
	}
	var out []obj
	if p := s.net.portOf(vm.ID); p != nil {
		out = append(out, obj{
			"port_id":    p.id,
			"net_id":     s.net.id,
			"mac_addr":   macFor(p.id),
			"port_state": "ACTIVE",
			"fixed_ips":  []obj{{"subnet_id": p.subnet.id, "ip_address": p.ip}},
		})
	}
	writeJSON(w, http.StatusOK, obj{"interfaceAttachments": list(out)})
}

func (s *server) serverActions(w http.ResponseWriter, r *http.Request, mv int) {
	vm := s.findVM(r.PathValue("id"))
	if vm == nil {
		s.serverNotFound(w, r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, obj{"instanceActions": []obj{{
		"action":        "create",
		"instance_uuid": vm.ID,
		"request_id":    "req-" + stableUUID("create-request", vm.ID),
		"user_id":       s.topo.UserID,
		"project_id":    s.topo.ProjectID,
		"start_time":    fmtTime(createdAt(vm.ID), layoutMicro),
		"message":       nil,
	}}})
}

func (s *server) serverMetadata(w http.ResponseWriter, r *http.Request, mv int) {
	if s.findVM(r.PathValue("id")) == nil {
		s.serverNotFound(w, r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, obj{"metadata": obj{}})
}

func extraSpecs(f *topology.Flavor) map[string]string {
	if f.ExtraSpecs == nil {
		return map[string]string{}
	}
	return f.ExtraSpecs
}

// flavorDetail always carries extra_specs, even below 2.61, so that a
// client reading /flavors/{id} without a microversion still sees the GPU
// properties (resources:VGPU, pci_passthrough:alias).
func (s *server) flavorDetail(f *topology.Flavor, mv int) obj {
	out := obj{
		"id":                         f.ID,
		"name":                       f.Name,
		"ram":                        f.RAMMB,
		"vcpus":                      f.VCPUs,
		"disk":                       f.DiskGB,
		"OS-FLV-EXT-DATA:ephemeral":  0,
		"swap":                       0,
		"rxtx_factor":                1.0,
		"OS-FLV-DISABLED:disabled":   false,
		"os-flavor-access:is_public": true,
		"links":                      s.flavorLinks(f.ID),
		"extra_specs":                extraSpecs(f),
	}
	if mv >= 55 {
		out["description"] = nil
	}
	return out
}

func (s *server) listFlavors(w http.ResponseWriter, r *http.Request, mv int) {
	var out []obj
	for _, f := range s.topo.Flavors {
		e := obj{"id": f.ID, "name": f.Name, "links": s.flavorLinks(f.ID)}
		if mv >= 55 {
			e["description"] = nil
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, obj{"flavors": list(out)})
}

func (s *server) listFlavorsDetail(w http.ResponseWriter, r *http.Request, mv int) {
	var out []obj
	for _, f := range s.topo.Flavors {
		out = append(out, s.flavorDetail(f, mv))
	}
	writeJSON(w, http.StatusOK, obj{"flavors": list(out)})
}

func (s *server) flavorOr404(w http.ResponseWriter, r *http.Request) *topology.Flavor {
	f := s.topo.FlavorByID(r.PathValue("id"))
	if f == nil {
		writeError(w, http.StatusNotFound, "itemNotFound", "Flavor "+r.PathValue("id")+" could not be found.")
	}
	return f
}

func (s *server) getFlavor(w http.ResponseWriter, r *http.Request, mv int) {
	if f := s.flavorOr404(w, r); f != nil {
		writeJSON(w, http.StatusOK, obj{"flavor": s.flavorDetail(f, mv)})
	}
}

func (s *server) flavorExtraSpecs(w http.ResponseWriter, r *http.Request, mv int) {
	if f := s.flavorOr404(w, r); f != nil {
		writeJSON(w, http.StatusOK, obj{"extra_specs": extraSpecs(f)})
	}
}

func (s *server) flavorExtraSpec(w http.ResponseWriter, r *http.Request, mv int) {
	f := s.flavorOr404(w, r)
	if f == nil {
		return
	}
	key := r.PathValue("key")
	v, ok := f.ExtraSpecs[key]
	if !ok {
		writeError(w, http.StatusNotFound, "itemNotFound", "Flavor "+f.ID+" has no extra specs with key "+key+".")
		return
	}
	writeJSON(w, http.StatusOK, obj{key: v})
}

func (s *server) flavorAccess(w http.ResponseWriter, r *http.Request, mv int) {
	if f := s.flavorOr404(w, r); f != nil {
		writeError(w, http.StatusNotFound, "itemNotFound", "Access list not available for public flavors.")
	}
}

func (s *server) computeServices(w http.ResponseWriter, r *http.Request, mv int) {
	updated := fmtTime(s.now(), "2006-01-02T15:04:05") + ".000000"
	svc := func(id int, binary, host, zone string) obj {
		var sid any = id
		if mv >= 53 {
			sid = stableUUID("nova-service", host, binary)
		}
		o := obj{
			"id": sid, "binary": binary, "host": host, "zone": zone,
			"status": "enabled", "state": "up", "updated_at": updated,
			"disabled_reason": nil,
		}
		if mv >= 11 {
			o["forced_down"] = false
		}
		return o
	}
	out := []obj{
		svc(1, "nova-conductor", controllerHost, "internal"),
		svc(2, "nova-scheduler", controllerHost, "internal"),
	}
	for i, h := range s.topo.Hypervisors {
		out = append(out, svc(computeServiceID(i), "nova-compute", h.Hostname, "nova"))
	}
	writeJSON(w, http.StatusOK, obj{"services": out})
}

func (s *server) availabilityZones(w http.ResponseWriter, r *http.Request, mv int) {
	writeJSON(w, http.StatusOK, obj{"availabilityZoneInfo": []obj{{
		"zoneName": "nova", "zoneState": obj{"available": true}, "hosts": nil,
	}}})
}

func (s *server) availabilityZonesDetail(w http.ResponseWriter, r *http.Request, mv int) {
	updated := fmtTime(s.now(), layoutMicro)
	state := func() obj { return obj{"available": true, "active": true, "updated_at": updated} }
	hosts := obj{}
	for _, h := range s.topo.Hypervisors {
		hosts[h.Hostname] = obj{"nova-compute": state()}
	}
	writeJSON(w, http.StatusOK, obj{"availabilityZoneInfo": []obj{
		{"zoneName": "internal", "zoneState": obj{"available": true}, "hosts": obj{
			controllerHost: obj{"nova-conductor": state(), "nova-scheduler": state()},
		}},
		{"zoneName": "nova", "zoneState": obj{"available": true}, "hosts": hosts},
	}})
}

var computeQuotaLimits = []struct {
	key   string
	limit int
}{
	{"cores", 2000},
	{"instances", 500},
	{"ram", 8192000},
	{"key_pairs", 100},
	{"metadata_items", 128},
	{"server_groups", 10},
	{"server_group_members", 10},
	{"injected_files", 5},
	{"injected_file_content_bytes", 10240},
	{"injected_file_path_bytes", 255},
}

func (s *server) totalVMUsage() (vcpus, ram, n int) {
	for _, vm := range s.topo.AllVMs() {
		vcpus += vm.VCPUs
		ram += vm.MemoryMB
		n++
	}
	return
}

func (s *server) computeQuota(w http.ResponseWriter, r *http.Request, mv int) {
	q := obj{"id": r.PathValue("project")}
	for _, l := range computeQuotaLimits {
		q[l.key] = l.limit
	}
	writeJSON(w, http.StatusOK, obj{"quota_set": q})
}

func (s *server) computeQuotaDetail(w http.ResponseWriter, r *http.Request, mv int) {
	vcpus, ram, n := s.totalVMUsage()
	used := map[string]int{"cores": vcpus, "ram": ram, "instances": n}
	q := obj{"id": r.PathValue("project")}
	for _, l := range computeQuotaLimits {
		q[l.key] = obj{"limit": l.limit, "in_use": used[l.key], "reserved": 0}
	}
	writeJSON(w, http.StatusOK, obj{"quota_set": q})
}

func (s *server) computeLimits(w http.ResponseWriter, r *http.Request, mv int) {
	vcpus, ram, n := s.totalVMUsage()
	writeJSON(w, http.StatusOK, obj{"limits": obj{
		"rate": []obj{},
		"absolute": obj{
			"maxTotalCores":           2000,
			"maxTotalInstances":       500,
			"maxTotalRAMSize":         8192000,
			"maxTotalKeypairs":        100,
			"maxServerMeta":           128,
			"maxServerGroups":         10,
			"maxServerGroupMembers":   10,
			"maxPersonality":          5,
			"maxPersonalitySize":      10240,
			"totalCoresUsed":          vcpus,
			"totalRAMUsed":            ram,
			"totalInstancesUsed":      n,
			"totalServerGroupsUsed":   0,
			"maxTotalFloatingIps":     -1,
			"totalFloatingIpsUsed":    0,
			"maxSecurityGroups":       -1,
			"totalSecurityGroupsUsed": 0,
			"maxSecurityGroupRules":   -1,
		},
	}})
}
