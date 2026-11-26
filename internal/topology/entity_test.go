package topology

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEntityID(t *testing.T) {
	a := EntityID("emu-vgpu-01", "")
	if a != StableUUID("entity", "emu-vgpu-01") {
		t.Fatalf("default entity id %s, want StableUUID(entity, name)", a)
	}
	if !uuidRE.MatchString(a) {
		t.Fatalf("default entity id not a UUID: %s", a)
	}
	if a == EntityID("emu-vgpu-02", "") {
		t.Fatal("two names gave the same entity id")
	}
	if a == StableUUID("hypervisor", "emu-vgpu-01") {
		t.Fatal("entity id equals the hypervisor uuid of the same name")
	}
	if got := EntityID("emu-vgpu-01", "my-node"); got != "my-node" {
		t.Fatalf("override ignored: %s", got)
	}
}

func TestEntityIDJSON(t *testing.T) {
	top := Default()
	raw, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "entity_id") {
		t.Fatal("empty entity_id was written to JSON")
	}
	top.Hypervisors[0].EntityID = "host-entity"
	top.Hypervisors[0].GPUs[0].VMs[0].EntityID = "vm-entity"
	raw, err = json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	var back Topology
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Hypervisors[0].EntityID; got != "host-entity" {
		t.Errorf("hypervisor entity_id after round trip %q", got)
	}
	if got := back.Hypervisors[0].GPUs[0].VMs[0].EntityID; got != "vm-entity" {
		t.Errorf("vm entity_id after round trip %q", got)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("topology with entity ids failed Validate: %v", err)
	}
}
