package tagrules

import "sort"

// KeyInfo describes one tag or label key a generator emits before any rule runs, for the settings screen.
type KeyInfo struct {
	Key     string   `json:"key"`
	Example string   `json:"example"` // the first non-empty value seen
	Where   []string `json:"where"`   // measurements (InfluxDB) or "resource" and "point" (OTLP), sorted
}

// KeySet collects KeyInfo while points are generated.
type KeySet struct {
	m map[string]*keyEntry
}

type keyEntry struct {
	example string
	where   map[string]bool
}

// Add records key with value seen in where. Empty values are ignored, as line protocol leaves such a tag out.
func (s *KeySet) Add(where, key, value string) {
	if value == "" {
		return
	}
	if s.m == nil {
		s.m = map[string]*keyEntry{}
	}
	e := s.m[key]
	if e == nil {
		e = &keyEntry{example: value, where: map[string]bool{}}
		s.m[key] = e
	}
	e.where[where] = true
}

// List returns the keys sorted by name. It is empty, not nil, when nothing was added.
func (s *KeySet) List() []KeyInfo {
	out := make([]KeyInfo, 0, len(s.m))
	for k, e := range s.m {
		w := make([]string, 0, len(e.where))
		for x := range e.where {
			w = append(w, x)
		}
		sort.Strings(w)
		out = append(out, KeyInfo{Key: k, Example: e.example, Where: w})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
