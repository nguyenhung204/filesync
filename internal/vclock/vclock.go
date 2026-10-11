// Package vclock implements version vectors: the sole ordering authority
// for sync decisions (ADR-003). It never uses wall-clock time.
//
// A VersionVector is NOT safe for concurrent use; callers must hold a lock.
package vclock

import (
	"encoding/json"
	"fmt"
)

// VersionVector maps device ID to a monotonically increasing counter.
// A missing key is equivalent to 0.
type VersionVector map[string]uint64

// Relation describes how two version vectors compare.
type Relation int

const (
	Equal Relation = iota
	// Dominates: v is >= other on every element and > on at least one.
	Dominates
	// Dominated: other dominates v.
	Dominated
	// Concurrent: each side is greater on at least one element (conflict).
	Concurrent
)

func (r Relation) String() string {
	switch r {
	case Equal:
		return "equal"
	case Dominates:
		return "dominates"
	case Dominated:
		return "dominated"
	case Concurrent:
		return "concurrent"
	default:
		return "unknown"
	}
}

// New returns an empty, non-nil vector.
func New() VersionVector {
	return VersionVector{}
}

// Get returns the counter for device, or 0 if absent.
func (v VersionVector) Get(device string) uint64 {
	return v[device]
}

// Increment adds 1 to device's counter, mutating v in place and returning it
// so calls can chain. If v is nil a new vector is created and returned, so
// callers must use the return value. An empty device is a no-op.
func (v VersionVector) Increment(device string) VersionVector {
	if device == "" {
		return v
	}
	if v == nil {
		v = New()
	}
	v[device]++
	return v
}

// Merge returns a NEW vector holding the per-element maximum of v and other.
// Neither input is mutated. Merge(nil) is a clone of v.
func (v VersionVector) Merge(other VersionVector) VersionVector {
	out := v.Clone()
	for device, n := range other {
		if n > out[device] {
			out[device] = n
		}
	}
	return out
}

// Compare relates v to other across the union of their keys.
func (v VersionVector) Compare(other VersionVector) Relation {
	greater, less := false, false
	for device, n := range v {
		if o := other.Get(device); n > o {
			greater = true
		} else if n < o {
			less = true
		}
	}
	for device, n := range other {
		if _, seen := v[device]; seen {
			continue
		}
		if n > 0 {
			less = true
		}
	}
	switch {
	case greater && less:
		return Concurrent
	case greater:
		return Dominates
	case less:
		return Dominated
	default:
		return Equal
	}
}

// HappenedBefore reports whether v is strictly dominated by other.
func (v VersionVector) HappenedBefore(other VersionVector) bool {
	return v.Compare(other) == Dominated
}

// Dominates reports whether v strictly dominates other.
func (v VersionVector) Dominates(other VersionVector) bool {
	return v.Compare(other) == Dominates
}

// Clone returns a deep copy. Cloning nil yields an empty non-nil vector.
func (v VersionVector) Clone() VersionVector {
	out := New()
	for device, n := range v {
		out[device] = n
	}
	return out
}

// Canonical returns a clone with all zero elements dropped.
func (v VersionVector) Canonical() VersionVector {
	out := New()
	for device, n := range v {
		if n != 0 {
			out[device] = n
		}
	}
	return out
}

// MarshalJSON encodes the canonical form; encoding/json sorts map keys, so
// marshaling the same vector twice yields identical bytes.
func (v VersionVector) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]uint64(v.Canonical()))
}

// UnmarshalJSON decodes a JSON object of device -> counter. null yields an
// empty vector. Non-object JSON, negative or fractional values, and empty
// device keys are rejected.
func (v *VersionVector) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*v = New()
		return nil
	}
	var raw map[string]uint64
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("vclock: unmarshal version vector: %w", err)
	}
	if raw == nil {
		return fmt.Errorf("vclock: unmarshal version vector: not a JSON object")
	}
	for device := range raw {
		if device == "" {
			return fmt.Errorf("vclock: unmarshal version vector: empty device id")
		}
	}
	*v = VersionVector(raw)
	return nil
}
