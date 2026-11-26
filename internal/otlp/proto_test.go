package otlp

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestMarshalGaugeKnownBytes compares a whole request with bytes worked out by hand from the .proto field numbers.
func TestMarshalGaugeKnownBytes(t *testing.T) {
	req := &ExportRequest{ResourceMetrics: []*ResourceMetrics{{
		ScopeMetrics: []*ScopeMetrics{{
			ScopeName: "s",
			Metrics: []*Metric{{
				Name: "m", Unit: "1", Type: Gauge,
				Points: []NumberDataPoint{{TimeUnixNano: 1, Int: 2, IsInt: true, Attributes: []KeyValue{String("k", "v")}}},
			}},
		}},
	}}}
	want := mustHex(t, ""+
		"0a2f"+ // resource_metrics, 47 bytes
		"122d"+ // scope_metrics, 45 bytes
		"0a030a0173"+ // scope{name:"s"}
		"1226"+ // metrics, 38 bytes
		"0a016d"+ // name "m"
		"1a0131"+ // unit "1"
		"2a1e"+ // gauge, 30 bytes
		"0a1c"+ // data_points, 28 bytes
		"190100000000000000"+ // time_unix_nano fixed64 1
		"310200000000000000"+ // as_int sfixed64 2
		"3a080a016b12030a0176") // attributes {key:"k" value{string_value:"v"}}
	if got := req.Marshal(); !bytes.Equal(got, want) {
		t.Fatalf("Marshal\n got %x\nwant %x", got, want)
	}
}

func TestMarshalSumKnownBytes(t *testing.T) {
	m := &Metric{
		Name: "c", Type: Sum, Monotonic: true,
		Points: []NumberDataPoint{{StartTimeUnixNano: 1, TimeUnixNano: 2, Double: 1.5, Attributes: []KeyValue{Int("xid", 79)}}},
	}
	want := mustHex(t, ""+
		"0a0163"+ // name "c"
		"3a2c"+ // sum, 44 bytes
		"0a26"+ // data_points, 38 bytes
		"110100000000000000"+ // start_time_unix_nano 1
		"190200000000000000"+ // time_unix_nano 2
		"21000000000000f83f"+ // as_double 1.5
		"3a090a037869641202184f"+ // attributes {key:"xid" value{int_value:79}}
		"1002"+ // aggregation_temporality CUMULATIVE
		"1801") // is_monotonic true
	if got := m.marshal(); !bytes.Equal(got, want) {
		t.Fatalf("marshal\n got %x\nwant %x", got, want)
	}
}

// field is one decoded protobuf field.
type field struct {
	num   int
	wire  int
	u     uint64 // varint or fixed64
	bytes []byte // length-delimited
}

// decode is a minimal protobuf decoder for the wire types the encoder writes.
func decode(t *testing.T, b []byte) []field {
	t.Helper()
	var out []field
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			t.Fatalf("bad tag at %x", b)
		}
		b = b[n:]
		f := field{num: int(tag >> 3), wire: int(tag & 7)}
		switch f.wire {
		case wireVarint:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				t.Fatalf("bad varint for field %d", f.num)
			}
			f.u, b = v, b[n:]
		case wireI64:
			if len(b) < 8 {
				t.Fatalf("short fixed64 for field %d", f.num)
			}
			f.u, b = binary.LittleEndian.Uint64(b), b[8:]
		case wireLen:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				t.Fatalf("bad length for field %d", f.num)
			}
			f.bytes, b = b[n:n+int(l)], b[n+int(l):]
		default:
			t.Fatalf("unexpected wire type %d", f.wire)
		}
		out = append(out, f)
	}
	return out
}

func only(t *testing.T, fs []field, num int) []field {
	t.Helper()
	var out []field
	for _, f := range fs {
		if f.num == num {
			out = append(out, f)
		}
	}
	return out
}

// decodedKV renders an attribute as key=value, with int values as i<value>.
func decodedKV(t *testing.T, b []byte) string {
	fs := decode(t, b)
	key := string(only(t, fs, 1)[0].bytes)
	v := decode(t, only(t, fs, 2)[0].bytes)
	if len(v) != 1 {
		t.Fatalf("AnyValue of %s has %d fields", key, len(v))
	}
	switch v[0].num {
	case 1:
		return key + "=" + string(v[0].bytes)
	case 3:
		return fmt.Sprintf("%s=i%d", key, int64(v[0].u))
	}
	t.Fatalf("AnyValue field %d", v[0].num)
	return ""
}

func TestMarshalDecodes(t *testing.T) {
	req := &ExportRequest{ResourceMetrics: []*ResourceMetrics{
		{
			Resource: []KeyValue{String("process.executable.name", "python3"), Int("process.pid", 4242)},
			ScopeMetrics: []*ScopeMetrics{{
				ScopeName: "scope-a", ScopeVersion: "1.0",
				Metrics: []*Metric{
					{Name: "g", Unit: "%", Type: Gauge, Points: []NumberDataPoint{
						{TimeUnixNano: 10, Double: 0, Attributes: []KeyValue{String("uuid", "GPU-1"), String("empty", "")}},
						{TimeUnixNano: 10, Double: -2.25},
					}},
					{Name: "u", Unit: "By", Type: Sum, Points: []NumberDataPoint{
						{StartTimeUnixNano: 5, TimeUnixNano: 10, Int: -7, IsInt: true, Attributes: []KeyValue{Int("neg", -1)}},
					}},
				},
			}},
		},
		{},
	}}
	top := decode(t, req.Marshal())
	if len(top) != 2 {
		t.Fatalf("resource_metrics %d, want 2 (an empty one included)", len(top))
	}
	if len(top[1].bytes) != 0 {
		t.Fatalf("empty ResourceMetrics encoded as %x", top[1].bytes)
	}
	rm := decode(t, top[0].bytes)
	res := decode(t, only(t, rm, 1)[0].bytes)
	var attrs []string
	for _, kv := range only(t, res, 1) {
		attrs = append(attrs, decodedKV(t, kv.bytes))
	}
	if fmt.Sprint(attrs) != "[process.executable.name=python3 process.pid=i4242]" {
		t.Fatalf("resource attributes %v", attrs)
	}
	sm := decode(t, only(t, rm, 2)[0].bytes)
	scope := decode(t, only(t, sm, 1)[0].bytes)
	if string(only(t, scope, 1)[0].bytes) != "scope-a" || string(only(t, scope, 2)[0].bytes) != "1.0" {
		t.Fatalf("scope %v", scope)
	}
	metrics := only(t, sm, 2)
	if len(metrics) != 2 {
		t.Fatalf("metrics %d", len(metrics))
	}

	g := decode(t, metrics[0].bytes)
	if string(only(t, g, 1)[0].bytes) != "g" || string(only(t, g, 3)[0].bytes) != "%" {
		t.Fatalf("gauge name/unit %v", g)
	}
	if len(only(t, g, 7)) != 0 {
		t.Fatal("gauge also has a sum")
	}
	gpts := only(t, decode(t, only(t, g, 5)[0].bytes), 1)
	if len(gpts) != 2 {
		t.Fatalf("gauge points %d", len(gpts))
	}
	p0 := decode(t, gpts[0].bytes)
	if d := only(t, p0, 4); len(d) != 1 || math.Float64frombits(d[0].u) != 0 {
		t.Fatalf("zero as_double must still be present: %v", p0)
	}
	if len(only(t, p0, 2)) != 0 {
		t.Fatal("zero start time was encoded")
	}
	var pattrs []string
	for _, kv := range only(t, p0, 7) {
		pattrs = append(pattrs, decodedKV(t, kv.bytes))
	}
	if fmt.Sprint(pattrs) != "[uuid=GPU-1 empty=]" {
		t.Fatalf("point attributes %v", pattrs)
	}
	p1 := decode(t, gpts[1].bytes)
	if v := math.Float64frombits(only(t, p1, 4)[0].u); v != -2.25 {
		t.Fatalf("as_double %v", v)
	}

	s := decode(t, metrics[1].bytes)
	sum := decode(t, only(t, s, 7)[0].bytes)
	if tmp := only(t, sum, 2); len(tmp) != 1 || tmp[0].u != 2 {
		t.Fatalf("aggregation_temporality %v", tmp)
	}
	if len(only(t, sum, 3)) != 0 {
		t.Fatal("is_monotonic false was encoded")
	}
	sp := decode(t, only(t, sum, 1)[0].bytes)
	if only(t, sp, 2)[0].u != 5 || only(t, sp, 3)[0].u != 10 {
		t.Fatalf("times %v", sp)
	}
	if v := int64(only(t, sp, 6)[0].u); v != -7 {
		t.Fatalf("as_int %d", v)
	}
	if kv := decodedKV(t, only(t, sp, 7)[0].bytes); kv != "neg=i-1" {
		t.Fatalf("negative int attribute %s", kv)
	}
}

func TestDataPoints(t *testing.T) {
	var nilReq *ExportRequest
	if nilReq.DataPoints() != 0 {
		t.Fatal("nil request has points")
	}
	req := &ExportRequest{ResourceMetrics: []*ResourceMetrics{
		{ScopeMetrics: []*ScopeMetrics{{Metrics: []*Metric{{Points: make([]NumberDataPoint, 3)}, {Points: make([]NumberDataPoint, 2)}}}}},
		{ScopeMetrics: []*ScopeMetrics{{Metrics: []*Metric{{Points: make([]NumberDataPoint, 4)}}}}},
	}}
	if got := req.DataPoints(); got != 9 {
		t.Fatalf("DataPoints %d, want 9", got)
	}
}
