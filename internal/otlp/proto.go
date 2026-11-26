// Package otlp builds OTLP metrics requests and sends them over OTLP/HTTP.
//
// The ExportMetricsServiceRequest protobuf is encoded by hand with the
// standard library, covering what the emulator needs: gauges and cumulative
// sums of number data points with string and integer attributes.
// Field numbers follow opentelemetry/proto/collector/metrics/v1/metrics_service.proto
// and opentelemetry/proto/metrics/v1/metrics.proto.
package otlp

import (
	"encoding/binary"
	"math"
)

// ExportRequest is an ExportMetricsServiceRequest.
type ExportRequest struct {
	ResourceMetrics []*ResourceMetrics
}

// ResourceMetrics is the metrics of one resource (one agent, or one process of an agent).
type ResourceMetrics struct {
	Resource     []KeyValue
	ScopeMetrics []*ScopeMetrics
}

// ScopeMetrics is the metrics of one instrumentation scope (one receiver or scraper).
type ScopeMetrics struct {
	ScopeName    string
	ScopeVersion string
	Metrics      []*Metric
}

// MetricType is the kind of data a Metric holds.
type MetricType int

const (
	// Gauge is a sampled value.
	Gauge MetricType = iota
	// Sum is a cumulative sum; Monotonic says whether it only grows (a counter).
	Sum
)

// Metric is one metric with its number data points.
// Sums are always sent with cumulative aggregation temporality.
type Metric struct {
	Name        string
	Description string
	Unit        string
	Type        MetricType
	Monotonic   bool
	Points      []NumberDataPoint
}

// NumberDataPoint is one value. IsInt picks Int (as_int) over Double (as_double).
type NumberDataPoint struct {
	Attributes        []KeyValue
	StartTimeUnixNano uint64
	TimeUnixNano      uint64
	Double            float64
	Int               int64
	IsInt             bool
}

// KeyValue is an attribute. IsInt picks Int (int_value) over Str (string_value).
type KeyValue struct {
	Key   string
	Str   string
	Int   int64
	IsInt bool
}

// String returns a string attribute.
func String(key, value string) KeyValue { return KeyValue{Key: key, Str: value} }

// Int returns an integer attribute.
func Int(key string, value int64) KeyValue { return KeyValue{Key: key, Int: value, IsInt: true} }

// DataPoints is the number of data points in the request.
func (r *ExportRequest) DataPoints() int {
	if r == nil {
		return 0
	}
	n := 0
	for _, rm := range r.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				n += len(m.Points)
			}
		}
	}
	return n
}

// Protobuf wire types.
const (
	wireVarint = 0
	wireI64    = 1
	wireLen    = 2
)

// aggregationTemporalityCumulative is AGGREGATION_TEMPORALITY_CUMULATIVE.
const aggregationTemporalityCumulative = 2

// Marshal encodes the request as protobuf. Fields holding their proto3
// default are left out, except the oneof members (the value of a data point
// and of an attribute), which must be present even when zero.
func (r *ExportRequest) Marshal() []byte {
	var b []byte
	for _, rm := range r.ResourceMetrics {
		b = appendMessage(b, 1, rm.marshal())
	}
	return b
}

func (rm *ResourceMetrics) marshal() []byte {
	var b []byte
	if len(rm.Resource) > 0 {
		var res []byte
		for _, kv := range rm.Resource {
			res = appendMessage(res, 1, kv.marshal())
		}
		b = appendMessage(b, 1, res)
	}
	for _, sm := range rm.ScopeMetrics {
		b = appendMessage(b, 2, sm.marshal())
	}
	return b
}

func (sm *ScopeMetrics) marshal() []byte {
	var b []byte
	if sm.ScopeName != "" || sm.ScopeVersion != "" {
		var scope []byte
		scope = appendString(scope, 1, sm.ScopeName)
		scope = appendString(scope, 2, sm.ScopeVersion)
		b = appendMessage(b, 1, scope)
	}
	for _, m := range sm.Metrics {
		b = appendMessage(b, 2, m.marshal())
	}
	return b
}

func (m *Metric) marshal() []byte {
	var b []byte
	b = appendString(b, 1, m.Name)
	b = appendString(b, 2, m.Description)
	b = appendString(b, 3, m.Unit)
	var data []byte
	for i := range m.Points {
		data = appendMessage(data, 1, m.Points[i].marshal())
	}
	switch m.Type {
	case Sum:
		data = appendTag(data, 2, wireVarint)
		data = binary.AppendUvarint(data, aggregationTemporalityCumulative)
		if m.Monotonic {
			data = appendTag(data, 3, wireVarint)
			data = binary.AppendUvarint(data, 1)
		}
		b = appendMessage(b, 7, data)
	default:
		b = appendMessage(b, 5, data)
	}
	return b
}

func (p *NumberDataPoint) marshal() []byte {
	var b []byte
	if p.StartTimeUnixNano != 0 {
		b = appendFixed64(b, 2, p.StartTimeUnixNano)
	}
	if p.TimeUnixNano != 0 {
		b = appendFixed64(b, 3, p.TimeUnixNano)
	}
	if p.IsInt {
		b = appendFixed64(b, 6, uint64(p.Int))
	} else {
		b = appendFixed64(b, 4, math.Float64bits(p.Double))
	}
	for _, kv := range p.Attributes {
		b = appendMessage(b, 7, kv.marshal())
	}
	return b
}

func (kv KeyValue) marshal() []byte {
	var b []byte
	b = appendString(b, 1, kv.Key)
	var v []byte
	if kv.IsInt {
		v = appendTag(v, 3, wireVarint)
		v = binary.AppendUvarint(v, uint64(kv.Int))
	} else {
		v = appendTag(v, 1, wireLen)
		v = binary.AppendUvarint(v, uint64(len(kv.Str)))
		v = append(v, kv.Str...)
	}
	return appendMessage(b, 2, v)
}

func appendTag(b []byte, field int, wire int) []byte {
	return binary.AppendUvarint(b, uint64(field)<<3|uint64(wire))
}

// appendMessage appends an embedded message, also when it is empty: an
// empty message is still an element of a repeated field.
func appendMessage(b []byte, field int, msg []byte) []byte {
	b = appendTag(b, field, wireLen)
	b = binary.AppendUvarint(b, uint64(len(msg)))
	return append(b, msg...)
}

// appendString appends a string field, left out when empty.
func appendString(b []byte, field int, s string) []byte {
	if s == "" {
		return b
	}
	b = appendTag(b, field, wireLen)
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func appendFixed64(b []byte, field int, v uint64) []byte {
	b = appendTag(b, field, wireI64)
	return binary.LittleEndian.AppendUint64(b, v)
}
