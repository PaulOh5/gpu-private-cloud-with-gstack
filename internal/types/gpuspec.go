package types

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseGPUSpec parses the `--gpu` flag value of `flex run`.
//
//	""  | "any"      → {AnyModel:true, Count:1}
//	"count:N"        → {AnyModel:true, Count:N}   (N >= 1)
//	"4090" | "a6000" → {Model:"4090", Count:1}    (substring match on GPU.Name)
//
// It is explicit about rejecting nonsense (empty count, count:0, negative)
// rather than silently defaulting — the user sees a clear error at submit time
// instead of a job that mysteriously never schedules.
func ParseGPUSpec(s string) (GPUSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "any") {
		return GPUSpec{AnyModel: true, Count: 1}, nil
	}
	if rest, ok := cutPrefixFold(s, "count:"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			return GPUSpec{}, fmt.Errorf("invalid gpu count %q: %w", rest, err)
		}
		if n < 1 {
			return GPUSpec{}, fmt.Errorf("gpu count must be >= 1, got %d", n)
		}
		return GPUSpec{AnyModel: true, Count: n}, nil
	}
	// Bare model substring (e.g. "4090"). Reject a stray "count:" typo'd as
	// "count" with no number rather than treating it as a model name.
	if strings.EqualFold(s, "count") {
		return GPUSpec{}, fmt.Errorf("gpu spec %q missing count, use count:N", s)
	}
	return GPUSpec{Model: s, Count: 1}, nil
}

// Matches reports whether g satisfies the spec's model constraint. Count and
// VRAM are enforced by the scheduler, not here.
func (spec GPUSpec) Matches(g GPU) bool {
	if spec.AnyModel || spec.Model == "" {
		return true
	}
	return strings.Contains(strings.ToLower(g.Name), strings.ToLower(spec.Model))
}

// cutPrefixFold is strings.CutPrefix with case-insensitive prefix matching.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}
