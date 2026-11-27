package metrics

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/jollaman999/give-me-real-gpu/internal/tagrules"
)

var (
	measurementEscaper = strings.NewReplacer(",", `\,`, " ", `\ `)
	keyEscaper         = strings.NewReplacer(",", `\,`, "=", `\=`, " ", `\ `)
	stringEscaper      = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
)

// batch collects line protocol lines in the order they are added.
type batch struct {
	lines []string
	rules []tagrules.Rule  // applied to the tags of every point
	keys  *tagrules.KeySet // when set, records the tags of every point before the rules
}

// add appends one point. The tag rules run on a copy of tags, empty tag values are dropped
// (InfluxDB rejects them), and a point without any field is skipped.
func (b *batch) add(name string, tags map[string]string, fields map[string]any, ts int64) {
	if b.keys != nil {
		for k, v := range tags {
			b.keys.Add(name, k, v)
		}
	}
	tags = tagrules.ApplyTags(b.rules, tags)
	if line := formatLine(name, tags, fields, ts); line != "" {
		b.lines = append(b.lines, line)
	}
}

// formatLine renders one point. Tag and field keys are sorted so the output is stable.
func formatLine(name string, tags map[string]string, fields map[string]any, ts int64) string {
	var sb strings.Builder
	sb.WriteString(measurementEscaper.Replace(name))

	tagKeys := make([]string, 0, len(tags))
	for k, v := range tags {
		if v != "" {
			tagKeys = append(tagKeys, k)
		}
	}
	sort.Strings(tagKeys)
	for _, k := range tagKeys {
		sb.WriteByte(',')
		sb.WriteString(keyEscaper.Replace(k))
		sb.WriteByte('=')
		sb.WriteString(keyEscaper.Replace(tags[k]))
	}

	fieldKeys := make([]string, 0, len(fields))
	for k := range fields {
		fieldKeys = append(fieldKeys, k)
	}
	sort.Strings(fieldKeys)
	n := 0
	for _, k := range fieldKeys {
		v, ok := formatFieldValue(fields[k])
		if !ok {
			continue
		}
		if n == 0 {
			sb.WriteByte(' ')
		} else {
			sb.WriteByte(',')
		}
		sb.WriteString(keyEscaper.Replace(k))
		sb.WriteByte('=')
		sb.WriteString(v)
		n++
	}
	if n == 0 {
		return ""
	}
	sb.WriteByte(' ')
	sb.WriteString(strconv.FormatInt(ts, 10))
	return sb.String()
}

// formatFieldValue renders a field value: int64/int as integer (i suffix),
// float64 as float, string quoted, bool as true/false. NaN and Inf are dropped.
func formatFieldValue(v any) (string, bool) {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10) + "i", true
	case int:
		return strconv.Itoa(x) + "i", true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "", false
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case string:
		return `"` + stringEscaper.Replace(x) + `"`, true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

// copyTags returns a copy of base with the given key/value pairs set.
func copyTags(base map[string]string, kv ...string) map[string]string {
	out := make(map[string]string, len(base)+len(kv)/2)
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}
