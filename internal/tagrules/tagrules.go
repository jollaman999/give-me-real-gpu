// Package tagrules changes the tag keys of a sink's points before they are written: InfluxDB line protocol tags
// and OTLP attributes. A sink has an ordered list of rules that rename a key, drop a key or add a fixed key=value.
// Measurement, field and metric names and the values of existing keys are never changed.
package tagrules

import (
	"fmt"
	"strings"

	"github.com/jollaman999/give-me-real-gpu/internal/otlp"
)

// Actions of a rule.
const (
	Rename = "rename" // move the value of Key to NewKey, replacing NewKey when it exists
	Drop   = "drop"   // remove Key
	Add    = "add"    // set Key to Value, replacing it when it exists
)

// MaxRules is the largest number of rules a sink can have.
const MaxRules = 50

// Rule is one step. Rules are applied in order, each to the result of the previous one.
type Rule struct {
	Action string `json:"action"`
	Key    string `json:"key"`
	NewKey string `json:"new_key,omitempty"` // rename only
	Value  string `json:"value,omitempty"`   // add only
}

// Validate returns every problem in rules, each prefixed with tag_rules[i].
func Validate(rules []Rule) []string {
	var p []string
	if len(rules) > MaxRules {
		p = append(p, fmt.Sprintf("tag_rules: %d rules, at most %d are allowed", len(rules), MaxRules))
	}
	for i, r := range rules {
		add := func(format string, a ...any) {
			p = append(p, fmt.Sprintf("tag_rules[%d]: ", i)+fmt.Sprintf(format, a...))
		}
		switch r.Action {
		case Rename, Drop, Add:
		default:
			add("action %q must be %q, %q or %q", r.Action, Rename, Drop, Add)
		}
		if blank(r.Key) {
			add("key is empty")
		}
		switch r.Action {
		case Rename:
			if blank(r.NewKey) {
				add("new_key is empty")
			} else if r.NewKey == r.Key {
				add("new_key %q is the same as key", r.NewKey)
			}
		case Add:
			if blank(r.Value) {
				add("value is empty (InfluxDB does not accept an empty tag value)")
			}
		}
		for _, f := range []struct{ name, v string }{{"key", r.Key}, {"new_key", r.NewKey}, {"value", r.Value}} {
			if strings.ContainsAny(f.v, "\r\n") {
				add("%s must not contain a line break", f.name)
			}
		}
	}
	return p
}

// Clean returns a copy of rules without the fields their action does not use (new_key outside rename,
// value outside add), so a value left over from switching the action is not stored. nil for no rules.
func Clean(rules []Rule) []Rule {
	if len(rules) == 0 {
		return nil
	}
	out := make([]Rule, len(rules))
	for i, r := range rules {
		if r.Action != Rename {
			r.NewKey = ""
		}
		if r.Action != Add {
			r.Value = ""
		}
		out[i] = r
	}
	return out
}

func blank(s string) bool {
	return strings.TrimSpace(s) == ""
}

// ApplyTags returns the tags after the rules. Without rules it returns tags itself; otherwise a new map,
// so a map shared by several points is not changed. A key with an empty value counts as absent,
// because line protocol leaves such a tag out: renaming it removes it and does not replace NewKey.
func ApplyTags(rules []Rule, tags map[string]string) map[string]string {
	if len(rules) == 0 {
		return tags
	}
	out := make(map[string]string, len(tags)+len(rules))
	for k, v := range tags {
		if v != "" {
			out[k] = v
		}
	}
	for _, r := range rules {
		switch r.Action {
		case Rename:
			if v, ok := out[r.Key]; ok {
				delete(out, r.Key)
				out[r.NewKey] = v
			}
		case Drop:
			delete(out, r.Key)
		case Add:
			out[r.Key] = r.Value
		}
	}
	return out
}

// ApplyRequest applies the rules to the attributes of every data point and every resource of req, in place.
// The attribute slices are replaced, not modified, so slices shared with the generator stay as they were.
// Add rules put the key on every data point; on a resource they only replace the value of a key that is
// already there, so a backend that turns resource attributes into labels does not get the key twice.
func ApplyRequest(rules []Rule, req *otlp.ExportRequest) {
	if len(rules) == 0 || req == nil {
		return
	}
	for _, rm := range req.ResourceMetrics {
		if len(rm.Resource) > 0 {
			rm.Resource = applyAttrs(rules, rm.Resource, false)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				for i := range m.Points {
					m.Points[i].Attributes = applyAttrs(rules, m.Points[i].Attributes, true)
				}
			}
		}
	}
}

// ApplyAttrs returns a new attribute list after the rules. Renamed attributes keep their place and type;
// added ones are string attributes appended at the end.
func ApplyAttrs(rules []Rule, attrs []otlp.KeyValue) []otlp.KeyValue {
	return applyAttrs(rules, attrs, true)
}

func applyAttrs(rules []Rule, attrs []otlp.KeyValue, addMissing bool) []otlp.KeyValue {
	out := append([]otlp.KeyValue(nil), attrs...)
	for _, r := range rules {
		switch r.Action {
		case Rename:
			if index(out, r.Key) < 0 {
				continue
			}
			out = without(out, r.NewKey)
			for i := range out {
				if out[i].Key == r.Key {
					out[i].Key = r.NewKey
				}
			}
		case Drop:
			out = without(out, r.Key)
		case Add:
			if index(out, r.Key) < 0 {
				if addMissing {
					out = append(out, otlp.String(r.Key, r.Value))
				}
				continue
			}
			for i := range out {
				if out[i].Key == r.Key {
					out[i] = otlp.String(r.Key, r.Value)
				}
			}
		}
	}
	return out
}

func index(attrs []otlp.KeyValue, key string) int {
	for i, kv := range attrs {
		if kv.Key == key {
			return i
		}
	}
	return -1
}

// without removes every attribute with key, reusing the backing array of attrs (always a private copy here).
func without(attrs []otlp.KeyValue, key string) []otlp.KeyValue {
	out := attrs[:0]
	for _, kv := range attrs {
		if kv.Key != key {
			out = append(out, kv)
		}
	}
	return out
}
