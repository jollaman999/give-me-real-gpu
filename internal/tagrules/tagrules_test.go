package tagrules

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jollaman999/give-me-real-gpu/internal/otlp"
)

func TestApplyTags(t *testing.T) {
	base := map[string]string{"host": "gpu-node-1", "hostname": "old", "time_diff": "0", "uuid": "u-1", "empty": ""}
	cases := []struct {
		name  string
		rules []Rule
		want  map[string]string
	}{
		{"rename replaces new_key", []Rule{{Action: Rename, Key: "host", NewKey: "hostname"}},
			map[string]string{"hostname": "gpu-node-1", "time_diff": "0", "uuid": "u-1"}},
		{"rename of a missing key keeps new_key", []Rule{{Action: Rename, Key: "nope", NewKey: "hostname"}},
			map[string]string{"host": "gpu-node-1", "hostname": "old", "time_diff": "0", "uuid": "u-1"}},
		{"rename of an empty value does not replace new_key", []Rule{{Action: Rename, Key: "empty", NewKey: "uuid"}},
			map[string]string{"host": "gpu-node-1", "hostname": "old", "time_diff": "0", "uuid": "u-1"}},
		{"drop", []Rule{{Action: Drop, Key: "time_diff"}, {Action: Drop, Key: "nope"}},
			map[string]string{"host": "gpu-node-1", "hostname": "old", "uuid": "u-1"}},
		{"add new and replace existing", []Rule{{Action: Add, Key: "env", Value: "dev"}, {Action: Add, Key: "uuid", Value: "u-2"}},
			map[string]string{"host": "gpu-node-1", "hostname": "old", "time_diff": "0", "uuid": "u-2", "env": "dev"}},
		{"in order: rename then drop the new key", []Rule{{Action: Rename, Key: "host", NewKey: "h"}, {Action: Drop, Key: "h"}},
			map[string]string{"hostname": "old", "time_diff": "0", "uuid": "u-1"}},
		{"in order: add then rename", []Rule{{Action: Add, Key: "env", Value: "dev"}, {Action: Rename, Key: "env", NewKey: "stage"}},
			map[string]string{"host": "gpu-node-1", "hostname": "old", "time_diff": "0", "uuid": "u-1", "stage": "dev"}},
		{"in order: rename then add the old key back", []Rule{{Action: Rename, Key: "host", NewKey: "node"}, {Action: Add, Key: "host", Value: "x"}},
			map[string]string{"node": "gpu-node-1", "host": "x", "hostname": "old", "time_diff": "0", "uuid": "u-1"}},
	}
	for _, tc := range cases {
		before := copyMap(base)
		got := ApplyTags(tc.rules, base)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		if !reflect.DeepEqual(base, before) {
			t.Errorf("%s: the input map was changed to %v", tc.name, base)
		}
	}
	if got := ApplyTags(nil, base); reflect.ValueOf(got).Pointer() != reflect.ValueOf(base).Pointer() {
		t.Error("without rules the same map must come back")
	}
}

func copyMap(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestApplyAttrs(t *testing.T) {
	base := []otlp.KeyValue{otlp.String("entity_id", "e-1"), otlp.Int("process.pid", 42), otlp.String("state", "user")}
	got := ApplyAttrs([]Rule{
		{Action: Rename, Key: "process.pid", NewKey: "pid"},
		{Action: Rename, Key: "state", NewKey: "entity_id"},
		{Action: Add, Key: "env", Value: "dev"},
		{Action: Drop, Key: "nope"},
	}, base)
	want := []otlp.KeyValue{otlp.Int("pid", 42), otlp.String("entity_id", "user"), otlp.String("env", "dev")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if base[0].Key != "entity_id" || base[1].Key != "process.pid" || len(base) != 3 {
		t.Errorf("input changed: %+v", base)
	}
	got = ApplyAttrs([]Rule{{Action: Add, Key: "process.pid", Value: "7"}, {Action: Drop, Key: "entity_id"}}, base)
	want = []otlp.KeyValue{otlp.String("process.pid", "7"), otlp.String("state", "user")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("add over an int attribute and drop: got %+v, want %+v", got, want)
	}
}

func TestApplyRequest(t *testing.T) {
	shared := []otlp.KeyValue{otlp.String("cpu", "cpu0"), otlp.String("entity_id", "e-1")}
	req := &otlp.ExportRequest{ResourceMetrics: []*otlp.ResourceMetrics{{
		Resource: []otlp.KeyValue{otlp.String("process.owner", "root"), otlp.String("env", "old")},
		ScopeMetrics: []*otlp.ScopeMetrics{{Metrics: []*otlp.Metric{{
			Name:   "system.cpu.time",
			Points: []otlp.NumberDataPoint{{Attributes: shared, Double: 1}, {Attributes: shared, Double: 2}},
		}}}},
	}, {
		ScopeMetrics: []*otlp.ScopeMetrics{{Metrics: []*otlp.Metric{{Name: "system.uptime", Points: []otlp.NumberDataPoint{{Double: 3}}}}}},
	}}}
	ApplyRequest([]Rule{
		{Action: Rename, Key: "entity_id", NewKey: "node_id"},
		{Action: Drop, Key: "process.owner"},
		{Action: Add, Key: "env", Value: "dev"},
	}, req)

	rm := req.ResourceMetrics[0]
	if want := []otlp.KeyValue{otlp.String("env", "dev")}; !reflect.DeepEqual(rm.Resource, want) {
		t.Errorf("resource %+v, want %+v", rm.Resource, want)
	}
	m := rm.ScopeMetrics[0].Metrics[0]
	want := []otlp.KeyValue{otlp.String("cpu", "cpu0"), otlp.String("node_id", "e-1"), otlp.String("env", "dev")}
	for i, p := range m.Points {
		if !reflect.DeepEqual(p.Attributes, want) {
			t.Errorf("point %d attributes %+v, want %+v", i, p.Attributes, want)
		}
	}
	if m.Name != "system.cpu.time" || m.Points[0].Double != 1 || m.Points[1].Double != 2 {
		t.Errorf("metric name or values changed: %+v", m)
	}
	if shared[1].Key != "entity_id" {
		t.Errorf("the generator's shared attribute slice was changed: %+v", shared)
	}
	rm2 := req.ResourceMetrics[1]
	if len(rm2.Resource) != 0 {
		t.Errorf("add put a key on an empty resource: %+v", rm2.Resource)
	}
	if want := []otlp.KeyValue{otlp.String("env", "dev")}; !reflect.DeepEqual(rm2.ScopeMetrics[0].Metrics[0].Points[0].Attributes, want) {
		t.Errorf("point without attributes got %+v", rm2.ScopeMetrics[0].Metrics[0].Points[0].Attributes)
	}

	before := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Points[0].Attributes
	ApplyRequest(nil, req)
	if !reflect.DeepEqual(req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Points[0].Attributes, before) {
		t.Error("no rules changed the request")
	}
	ApplyRequest([]Rule{{Action: Drop, Key: "x"}}, nil)
}

func TestValidate(t *testing.T) {
	if p := Validate(nil); len(p) != 0 {
		t.Errorf("no rules: %q", p)
	}
	good := []Rule{
		{Action: Rename, Key: "host", NewKey: "hostname"},
		{Action: Drop, Key: "time_diff"},
		{Action: Add, Key: "env", Value: "dev"},
	}
	if p := Validate(good); len(p) != 0 {
		t.Errorf("good rules: %q", p)
	}
	bad := []Rule{
		{Action: "replace", Key: "a"},
		{Action: Drop, Key: " "},
		{Action: Rename, Key: "a"},
		{Action: Rename, Key: "a", NewKey: "a"},
		{Action: Add, Key: "env"},
		{Action: Add, Key: "env\n", Value: "dev\r"},
		{Action: Rename, Key: "a", NewKey: "b\nc"},
	}
	want := []string{
		`tag_rules[0]: action "replace" must be "rename", "drop" or "add"`,
		"tag_rules[1]: key is empty",
		"tag_rules[2]: new_key is empty",
		`tag_rules[3]: new_key "a" is the same as key`,
		"tag_rules[4]: value is empty (InfluxDB does not accept an empty tag value)",
		"tag_rules[5]: key must not contain a line break",
		"tag_rules[5]: value must not contain a line break",
		"tag_rules[6]: new_key must not contain a line break",
	}
	if p := Validate(bad); !reflect.DeepEqual(p, want) {
		t.Errorf("problems\n%s\nwant\n%s", strings.Join(p, "\n"), strings.Join(want, "\n"))
	}

	many := make([]Rule, MaxRules)
	for i := range many {
		many[i] = Rule{Action: Drop, Key: "k"}
	}
	if p := Validate(many); len(p) != 0 {
		t.Errorf("%d rules: %q", MaxRules, p)
	}
	many = append(many, Rule{Action: Drop, Key: "k"})
	if p := Validate(many); len(p) != 1 || p[0] != "tag_rules: 51 rules, at most 50 are allowed" {
		t.Errorf("%d rules: %q", len(many), p)
	}
}

func TestClean(t *testing.T) {
	if Clean(nil) != nil || Clean([]Rule{}) != nil {
		t.Error("no rules must clean to nil")
	}
	in := []Rule{
		{Action: Drop, Key: "a", NewKey: "x", Value: "y"},
		{Action: Rename, Key: "a", NewKey: "b", Value: "y"},
		{Action: Add, Key: "a", NewKey: "x", Value: "y"},
	}
	want := []Rule{{Action: Drop, Key: "a"}, {Action: Rename, Key: "a", NewKey: "b"}, {Action: Add, Key: "a", Value: "y"}}
	if got := Clean(in); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if in[0].NewKey != "x" {
		t.Error("Clean changed its input")
	}
}
