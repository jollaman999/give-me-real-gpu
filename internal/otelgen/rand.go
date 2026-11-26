package otelgen

import (
	"hash/fnv"
	"math"
	"math/rand"
)

// newRand returns a random source derived from the seed and a stable entity key,
// so adding or removing one entity does not shift the values of the others.
func newRand(seed int64, key string) *rand.Rand {
	return rand.New(rand.NewSource(seed ^ int64(hashKey(key))))
}

// hashKey is a stable hash used for per-entity choices that must not consume randomness.
func hashKey(key string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(key))
	return h.Sum64()
}

// walk is a mean-reverting random walk inside [lo, hi].
// The target occasionally jumps to a new level, which looks like a workload starting or stopping.
type walk struct {
	v, target  float64
	lo, hi     float64
	step, jump float64
	idle       float64 // probability that a new target is near lo
}

func newWalk(r *rand.Rand, lo, hi, step, jump float64) *walk {
	w := &walk{lo: lo, hi: hi, step: step, jump: jump, idle: 0.35}
	w.target = w.pick(r)
	w.v = w.target
	return w
}

func (w *walk) pick(r *rand.Rand) float64 {
	span := w.hi - w.lo
	if r.Float64() < w.idle {
		return w.lo + span*0.05*r.Float64()
	}
	return w.lo + span*(0.15+0.8*r.Float64())
}

func (w *walk) next(r *rand.Rand) float64 {
	if r.Float64() < w.jump {
		w.target = w.pick(r)
	}
	w.v += (w.target-w.v)*0.3 + r.NormFloat64()*w.step
	w.v = clamp(w.v, w.lo, w.hi)
	return w.v
}

// counter is a cumulative value that never decreases.
type counter struct {
	v float64
}

// add grows the counter by rate*dt scaled by a random factor in [0.5, 1.5) and returns the new value.
func (c *counter) add(r *rand.Rand, rate, dt float64) float64 {
	if dt > 0 && rate > 0 {
		c.v += rate * dt * (0.5 + r.Float64())
	}
	return c.v
}

// bump adds one with probability p. Used for rare error and drop counters.
func (c *counter) bump(r *rand.Rand, p float64) float64 {
	if r.Float64() < p {
		c.v++
	}
	return c.v
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}
