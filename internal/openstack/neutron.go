package openstack

import (
	"net/http"
	"net/netip"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// netModel is the single tenant network every VM is attached to. Each /24
// that holds a VM address becomes one subnet, and each VM gets one port.
type netModel struct {
	id, name string
	subnets  []*subnet
	ports    []*port
	byVM     map[string]*port
	sgID     string
}

type subnet struct {
	id, name string
	prefix   netip.Prefix
}

type port struct {
	id     string
	vm     *topology.VM
	ip     string
	subnet *subnet
}

func portID(vmID string) string { return stableUUID("port", vmID) }

func buildNetModel(t *topology.Topology) *netModel {
	n := &netModel{
		id:   stableUUID("network", t.ProjectID, "emu-net"),
		name: "emu-net",
		byVM: map[string]*port{},
		sgID: stableUUID("security-group", t.ProjectID, "default"),
	}
	byPrefix := map[netip.Prefix]*subnet{}
	for _, vm := range t.AllVMs() {
		addr, err := netip.ParseAddr(vm.IP)
		if err != nil || !addr.Is4() {
			continue
		}
		pfx := netip.PrefixFrom(addr, 24).Masked()
		sn := byPrefix[pfx]
		if sn == nil {
			sn = &subnet{id: stableUUID("subnet", pfx.String()), name: "emu-subnet-" + itoa(len(n.subnets)+1), prefix: pfx}
			byPrefix[pfx] = sn
			n.subnets = append(n.subnets, sn)
		}
		p := &port{id: portID(vm.ID), vm: vm, ip: vm.IP, subnet: sn}
		n.ports = append(n.ports, p)
		n.byVM[vm.ID] = p
	}
	return n
}

func (n *netModel) portOf(vmID string) *port { return n.byVM[vmID] }

func (s *server) neutronRoutes() {
	m := s.mux
	const p = "/network/v2.0"
	m.HandleFunc("GET /network", s.neutronVersions)
	m.HandleFunc("GET /network/{$}", s.neutronVersions)
	m.HandleFunc("GET "+p, s.neutronResources)
	m.HandleFunc("GET "+p+"/{$}", s.neutronResources)
	m.HandleFunc("GET "+p+"/networks", s.listNetworks)
	m.HandleFunc("GET "+p+"/networks/{id}", s.getNetwork)
	m.HandleFunc("GET "+p+"/networks/{id}/dhcp-agents", s.networkDHCPAgents)
	m.HandleFunc("GET "+p+"/subnets", s.listSubnets)
	m.HandleFunc("GET "+p+"/subnets/{id}", s.getSubnet)
	m.HandleFunc("GET "+p+"/ports", s.listPorts)
	m.HandleFunc("GET "+p+"/ports/{id}", s.getPort)
	m.HandleFunc("GET "+p+"/security-groups", s.listSecurityGroups)
	m.HandleFunc("GET "+p+"/security-groups/{id}", s.getSecurityGroup)
	m.HandleFunc("GET "+p+"/security-group-rules", s.listSecurityGroupRules)
	m.HandleFunc("GET "+p+"/agents", s.listAgents)
	m.HandleFunc("GET "+p+"/availability_zones", s.neutronAZs)
	m.HandleFunc("GET "+p+"/network-ip-availabilities/{id}", s.networkIPAvailability)
	m.HandleFunc("GET "+p+"/quotas/{project}", s.neutronQuota)
	m.HandleFunc("GET "+p+"/quotas/{project}/default", s.neutronQuota)
	m.HandleFunc("GET "+p+"/quotas/{project}/details", s.neutronQuotaDetails)
	for path, key := range map[string]string{
		"/routers":                "routers",
		"/floatingips":            "floatingips",
		"/qos/policies":           "policies",
		"/qos/rule-types":         "rule_types",
		"/rbac-policies":          "rbac_policies",
		"/trunks":                 "trunks",
		"/extensions":             "extensions",
		"/address-scopes":         "address_scopes",
		"/subnetpools":            "subnetpools",
		"/network_segment_ranges": "network_segment_ranges",
	} {
		key := key
		m.HandleFunc("GET "+p+path, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, obj{key: []obj{}})
		})
	}
}

func (s *server) neutronVersions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"versions": []obj{{
		"id": "v2.0", "status": "CURRENT",
		"links": []obj{{"rel": "self", "href": requestBase(r) + "/network/v2.0/"}},
	}}})
}

func (s *server) neutronResources(w http.ResponseWriter, r *http.Request) {
	var out []obj
	for _, c := range []struct{ name, coll string }{
		{"network", "networks"}, {"subnet", "subnets"}, {"port", "ports"},
		{"security_group", "security-groups"}, {"router", "routers"}, {"floatingip", "floatingips"},
	} {
		out = append(out, obj{"name": c.name, "collection": c.coll,
			"links": []obj{{"rel": "self", "href": requestBase(r) + "/network/v2.0/" + c.coll}}})
	}
	writeJSON(w, http.StatusOK, obj{"resources": out})
}

func (s *server) neutronNotFound(w http.ResponseWriter, kind, id string) {
	writeJSON(w, http.StatusNotFound, obj{"NeutronError": obj{
		"type": kind + "NotFound", "message": kind + " " + id + " could not be found.", "detail": "",
	}})
}

func (s *server) networkJSON() obj {
	created := fmtTime(createdAt(s.net.id), layoutISOZ)
	var subnetIDs []string
	for _, sn := range s.net.subnets {
		subnetIDs = append(subnetIDs, sn.id)
	}
	return obj{
		"id":                        s.net.id,
		"name":                      s.net.name,
		"status":                    "ACTIVE",
		"admin_state_up":            true,
		"shared":                    false,
		"router:external":           false,
		"subnets":                   list(subnetIDs),
		"tenant_id":                 s.topo.ProjectID,
		"project_id":                s.topo.ProjectID,
		"provider:network_type":     "vxlan",
		"provider:physical_network": nil,
		"provider:segmentation_id":  1001,
		"mtu":                       1450,
		"port_security_enabled":     true,
		"availability_zone_hints":   []string{},
		"availability_zones":        []string{"nova"},
		"ipv4_address_scope":        nil,
		"ipv6_address_scope":        nil,
		"description":               "",
		"tags":                      []string{},
		"created_at":                created,
		"updated_at":                created,
		"revision_number":           2,
	}
}

func (s *server) listNetworks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"networks": []obj{s.networkJSON()}})
}

func (s *server) getNetwork(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != s.net.id {
		s.neutronNotFound(w, "Network", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, obj{"network": s.networkJSON()})
}

func (s *server) subnetJSON(sn *subnet) obj {
	base := sn.prefix.Addr().As4()
	ip := func(last byte) string { b := base; b[3] = last; return netip.AddrFrom4(b).String() }
	created := fmtTime(createdAt(sn.id), layoutISOZ)
	return obj{
		"id":                sn.id,
		"name":              sn.name,
		"network_id":        s.net.id,
		"tenant_id":         s.topo.ProjectID,
		"project_id":        s.topo.ProjectID,
		"ip_version":        4,
		"cidr":              sn.prefix.String(),
		"gateway_ip":        ip(1),
		"enable_dhcp":       true,
		"allocation_pools":  []obj{{"start": ip(2), "end": ip(254)}},
		"dns_nameservers":   []string{},
		"host_routes":       []obj{},
		"ipv6_address_mode": nil,
		"ipv6_ra_mode":      nil,
		"subnetpool_id":     nil,
		"description":       "",
		"tags":              []string{},
		"created_at":        created,
		"updated_at":        created,
		"revision_number":   0,
	}
}

func (s *server) listSubnets(w http.ResponseWriter, r *http.Request) {
	var out []obj
	for _, sn := range s.net.subnets {
		out = append(out, s.subnetJSON(sn))
	}
	writeJSON(w, http.StatusOK, obj{"subnets": list(out)})
}

func (s *server) getSubnet(w http.ResponseWriter, r *http.Request) {
	for _, sn := range s.net.subnets {
		if sn.id == r.PathValue("id") {
			writeJSON(w, http.StatusOK, obj{"subnet": s.subnetJSON(sn)})
			return
		}
	}
	s.neutronNotFound(w, "Subnet", r.PathValue("id"))
}

func (s *server) portJSON(p *port) obj {
	created := fmtTime(createdAt(p.vm.ID), layoutISOZ)
	return obj{
		"id":                    p.id,
		"name":                  "",
		"network_id":            s.net.id,
		"tenant_id":             s.topo.ProjectID,
		"project_id":            s.topo.ProjectID,
		"mac_address":           macFor(p.id),
		"admin_state_up":        true,
		"status":                "ACTIVE",
		"device_id":             p.vm.ID,
		"device_owner":          "compute:nova",
		"fixed_ips":             []obj{{"subnet_id": p.subnet.id, "ip_address": p.ip}},
		"allowed_address_pairs": []obj{},
		"extra_dhcp_opts":       []obj{},
		"security_groups":       []string{s.net.sgID},
		"port_security_enabled": true,
		"binding:host_id":       p.vm.Hypervisor,
		"binding:vif_type":      "ovs",
		"binding:vnic_type":     "normal",
		"binding:vif_details":   obj{"port_filter": true, "connectivity": "l2", "bridge_name": "br-int"},
		"binding:profile":       obj{},
		"description":           "",
		"tags":                  []string{},
		"created_at":            created,
		"updated_at":            created,
		"revision_number":       4,
	}
}

func (s *server) listPorts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var out []obj
	for _, p := range s.net.ports {
		if dev := q.Get("device_id"); dev != "" && dev != p.vm.ID {
			continue
		}
		if nid := q.Get("network_id"); nid != "" && nid != s.net.id {
			continue
		}
		out = append(out, s.portJSON(p))
	}
	writeJSON(w, http.StatusOK, obj{"ports": list(out)})
}

func (s *server) getPort(w http.ResponseWriter, r *http.Request) {
	for _, p := range s.net.ports {
		if p.id == r.PathValue("id") {
			writeJSON(w, http.StatusOK, obj{"port": s.portJSON(p)})
			return
		}
	}
	s.neutronNotFound(w, "Port", r.PathValue("id"))
}

func (s *server) securityGroupRules() []obj {
	rule := func(dir, ether string, remoteGroup any) obj {
		return obj{
			"id":                stableUUID("sg-rule", s.net.sgID, dir, ether),
			"security_group_id": s.net.sgID,
			"tenant_id":         s.topo.ProjectID,
			"project_id":        s.topo.ProjectID,
			"direction":         dir,
			"ethertype":         ether,
			"protocol":          nil,
			"port_range_min":    nil,
			"port_range_max":    nil,
			"remote_ip_prefix":  nil,
			"remote_group_id":   remoteGroup,
			"description":       "",
		}
	}
	return []obj{
		rule("egress", "IPv4", nil),
		rule("egress", "IPv6", nil),
		rule("ingress", "IPv4", s.net.sgID),
		rule("ingress", "IPv6", s.net.sgID),
	}
}

func (s *server) securityGroupJSON() obj {
	return obj{
		"id":                   s.net.sgID,
		"name":                 "default",
		"description":          "Default security group",
		"tenant_id":            s.topo.ProjectID,
		"project_id":           s.topo.ProjectID,
		"stateful":             true,
		"security_group_rules": s.securityGroupRules(),
		"tags":                 []string{},
	}
}

func (s *server) listSecurityGroups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"security_groups": []obj{s.securityGroupJSON()}})
}

func (s *server) getSecurityGroup(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != s.net.sgID {
		s.neutronNotFound(w, "SecurityGroup", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, obj{"security_group": s.securityGroupJSON()})
}

func (s *server) listSecurityGroupRules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"security_group_rules": s.securityGroupRules()})
}

func (s *server) agentJSON(typ, binary, host, topic string, az any) obj {
	now := fmtTime(s.now(), layoutAgentTime)
	since := fmtTime(createdAt(host), layoutAgentTime)
	return obj{
		"id":                  stableUUID("agent", host, binary),
		"agent_type":          typ,
		"binary":              binary,
		"host":                host,
		"topic":               topic,
		"availability_zone":   az,
		"admin_state_up":      true,
		"alive":               true,
		"heartbeat_timestamp": now,
		"created_at":          since,
		"started_at":          since,
		"description":         nil,
		"configurations":      obj{},
	}
}

func (s *server) dhcpAgent() obj {
	return s.agentJSON("DHCP agent", "neutron-dhcp-agent", controllerHost, "dhcp_agent", "nova")
}

func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	out := []obj{
		s.dhcpAgent(),
		s.agentJSON("L3 agent", "neutron-l3-agent", controllerHost, "l3_agent", "nova"),
		s.agentJSON("Metadata agent", "neutron-metadata-agent", controllerHost, "N/A", nil),
		s.agentJSON("Open vSwitch agent", "neutron-openvswitch-agent", controllerHost, "N/A", nil),
	}
	for _, h := range s.topo.Hypervisors {
		out = append(out, s.agentJSON("Open vSwitch agent", "neutron-openvswitch-agent", h.Hostname, "N/A", nil))
	}
	writeJSON(w, http.StatusOK, obj{"agents": out})
}

func (s *server) networkDHCPAgents(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != s.net.id {
		s.neutronNotFound(w, "Network", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, obj{"agents": []obj{s.dhcpAgent()}})
}

func (s *server) neutronAZs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"availability_zones": []obj{
		{"name": "nova", "resource": "network", "state": "available"},
		{"name": "nova", "resource": "router", "state": "available"},
	}})
}

func (s *server) networkIPAvailability(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != s.net.id {
		s.neutronNotFound(w, "Network", r.PathValue("id"))
		return
	}
	used := map[string]int{}
	for _, p := range s.net.ports {
		used[p.subnet.id]++
	}
	var subs []obj
	total, usedAll := 0, 0
	for _, sn := range s.net.subnets {
		// pool .2-.254 plus the DHCP port
		u := used[sn.id] + 1
		subs = append(subs, obj{
			"subnet_id": sn.id, "subnet_name": sn.name, "cidr": sn.prefix.String(),
			"ip_version": 4, "total_ips": 253, "used_ips": u,
		})
		total += 253
		usedAll += u
	}
	writeJSON(w, http.StatusOK, obj{"network_ip_availability": obj{
		"network_id": s.net.id, "network_name": s.net.name,
		"tenant_id": s.topo.ProjectID, "project_id": s.topo.ProjectID,
		"total_ips": total, "used_ips": usedAll, "subnet_ip_availability": list(subs),
	}})
}

var neutronQuotaLimits = []struct {
	key   string
	limit int
}{
	{"network", 100}, {"subnet", 100}, {"port", 500}, {"router", 10},
	{"floatingip", 50}, {"security_group", 10}, {"security_group_rule", 100}, {"rbac_policy", 10},
}

func (s *server) neutronQuota(w http.ResponseWriter, r *http.Request) {
	q := obj{}
	for _, l := range neutronQuotaLimits {
		q[l.key] = l.limit
	}
	writeJSON(w, http.StatusOK, obj{"quota": q})
}

func (s *server) neutronQuotaDetails(w http.ResponseWriter, r *http.Request) {
	used := map[string]int{
		"network": 1, "subnet": len(s.net.subnets), "port": len(s.net.ports) + 1,
		"security_group": 1, "security_group_rule": 4,
	}
	q := obj{}
	for _, l := range neutronQuotaLimits {
		q[l.key] = obj{"limit": l.limit, "used": used[l.key], "reserved": 0}
	}
	writeJSON(w, http.StatusOK, obj{"quota": q})
}
