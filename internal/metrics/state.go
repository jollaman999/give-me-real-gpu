package metrics

import (
	"hash/fnv"
	"math"
	"math/rand"
)

// newRand returns a random source derived from the seed and a stable entity key,
// so adding or removing one entity does not shift the values of the others.
func newRand(seed int64, key string) *rand.Rand {
	h := fnv.New64a()
	h.Write([]byte(key))
	return rand.New(rand.NewSource(seed ^ int64(h.Sum64())))
}

// hashKey is a stable small hash used for per-entity choices that must not consume randomness.
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
}

func newWalk(r *rand.Rand, lo, hi, step, jump float64) *walk {
	w := &walk{lo: lo, hi: hi, step: step, jump: jump}
	w.target = w.pick(r)
	w.v = w.target
	return w
}

// pick chooses a new target level. Roughly a third of the time the entity is idle.
func (w *walk) pick(r *rand.Rand) float64 {
	span := w.hi - w.lo
	if r.Float64() < 0.35 {
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

// counter is a monotonically increasing cumulative value (bytes, packets, ms).
type counter struct {
	v float64
}

// add grows the counter by rate*dt scaled by a random factor in [0.5, 1.5) and returns the new value.
func (c *counter) add(r *rand.Rand, rate, dt float64) int64 {
	if dt > 0 && rate > 0 {
		c.v += rate * dt * (0.5 + r.Float64())
	}
	return int64(c.v)
}

// bump adds one with probability p per collection. Used for rare error/drop counters.
func (c *counter) bump(r *rand.Rand, p float64) int64 {
	if r.Float64() < p {
		c.v++
	}
	return int64(c.v)
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}
