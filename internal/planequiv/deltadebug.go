package planequiv

import (
	"context"
	"maps"
	"slices"

	"github.com/authzed/spicedb/pkg/tuple"
)

// reductionHarness drives deletion-only reduction against the real executor.
// Every rerun rebuilds a fresh in-process model store and replays both plans,
// so a retained reduction is backed by an observed semantic difference rather
// than a static guess. The rerun budget is shared by both reduction axes.
type reductionHarness struct {
	v       *PlanEquivalenceValidator
	ctx     context.Context
	input   Input
	request Request

	caveatContext map[string]any
	remaining     int
	reruns        int
	capped        bool
	lastDiverging *tripleOutcome
}

// spend consumes one paired-execution rerun. When the budget is exhausted the
// caller must stop reducing; no further reruns are issued.
func (h *reductionHarness) spend() bool {
	if h.remaining <= 0 {
		h.capped = true
		return false
	}
	h.remaining--
	h.reruns++
	return true
}

// diverges reruns the full executor pair on the candidate reduced input and
// reports whether the original semantic difference still reproduces. It never
// simulates outcomes: both plans are compiled and executed again.
func (h *reductionHarness) diverges(relationships []tuple.Relationship, caveatContext map[string]any) bool {
	store, err := buildModelStore(h.ctx, h.input.Schema, h.input.CaveatDefinitions, relationships)
	if err != nil {
		// A reduction that breaks model setup is not a valid retained input.
		return false
	}
	defer store.close()

	schemaReader, err := store.reader.ReadSchema(h.ctx)
	if err != nil {
		return false
	}

	outcome := h.v.runTriple(h.ctx, store.reader, schemaReader, h.input, h.request, caveatContext)
	if outcome.canceled {
		return false
	}
	if outcome.mismatch != nil {
		// Keep the confirmed-diverging outcome so the final artifact can render
		// the side outputs produced by the retained reduced input.
		h.lastDiverging = &outcome
		return true
	}
	return false
}

// reduceRelationships applies the classic ddmin loop over the relationship
// list. It only ever deletes: each candidate is the current input minus one
// chunk, and a deletion is kept solely when the divergence survives a rerun.
// The result is irreducible under the bounded search, not globally minimal.
func (h *reductionHarness) reduceRelationships(relationships []tuple.Relationship) []tuple.Relationship {
	current := slices.Clone(relationships)
	if len(current) < 2 {
		return current
	}

	granularity := 2
	for len(current) >= 2 {
		chunks := partition(len(current), granularity)
		reduced := false
		for _, chunk := range chunks {
			if !h.spend() {
				return current
			}
			complement := withoutRange(current, chunk.start, chunk.end)
			if h.diverges(complement, h.caveatContext) {
				current = complement
				granularity = max(granularity-1, 2)
				reduced = true
				break
			}
		}
		if reduced {
			continue
		}
		if granularity < len(current) {
			granularity = min(2*granularity, len(current))
			continue
		}
		break
	}
	return current
}

// reduceContextFields greedily removes one top-level caveat context field at
// a time, keeping a removal only when the divergence survives the rerun.
// Nested fields are deliberately left untouched: the reduction contract is
// limited to relationship and top-level input field deletion.
func (h *reductionHarness) reduceContextFields(relationships []tuple.Relationship) (map[string]any, int) {
	current := maps.Clone(h.caveatContext)
	removed := 0
	for _, key := range slices.Sorted(maps.Keys(current)) {
		if !h.spend() {
			return current, removed
		}
		candidate := maps.Clone(current)
		delete(candidate, key)
		if h.diverges(relationships, candidate) {
			delete(current, key)
			removed++
		}
	}
	return current, removed
}

// chunkRange is a half-open index interval [start, end) within a slice.
type chunkRange struct {
	start int
	end   int
}

// partition splits a length-n slice into count balanced contiguous chunks.
func partition(n, count int) []chunkRange {
	if count > n {
		count = n
	}
	chunks := make([]chunkRange, 0, count)
	base := n / count
	extra := n % count
	start := 0
	for i := 0; i < count; i++ {
		size := base
		if i < extra {
			size++
		}
		chunks = append(chunks, chunkRange{start: start, end: start + size})
		start += size
	}
	return chunks
}

// withoutRange returns a copy of values with the half-open range removed.
func withoutRange(values []tuple.Relationship, start, end int) []tuple.Relationship {
	result := make([]tuple.Relationship, 0, len(values)-(end-start))
	result = append(result, values[:start]...)
	result = append(result, values[end:]...)
	return result
}
