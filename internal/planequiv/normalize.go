package planequiv

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/authzed/spicedb/internal/caveats"
	"github.com/authzed/spicedb/pkg/caveats/types"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/query"
	"github.com/authzed/spicedb/pkg/tuple"
)

// semanticEntry is the internal, classification-rich form of one result cell.
// It is never serialized directly; toRecord renders its JSON-safe form.
type semanticEntry struct {
	class       Classification
	missingVars []string
	exprText    string
	message     string
}

// noEntry is the explicit NO cell materialized for keys that only one side
// produces, so a missing grant is a visible mismatch rather than an implicit
// absence.
var noEntry = semanticEntry{class: ClassNo}

// entryDiff describes the first mismatching cell between two sides.
type entryDiff struct {
	key    string
	left   semanticEntry
	right  semanticEntry
	reason string
}

// entryKey builds the order-independent identity of a membership cell.
//
//   - Check collapses to the single key "check": only the asked pair matters.
//   - IterResources keys on the resource/relation pair: every returned path is
//     evidence that the single anchor subject reaches that resource, regardless
//     of which userset or wildcard produced it.
//   - IterSubjects keys on the full resource#relation@subject ONR, including
//     the expanded wildcard domain.
func entryKey(op query.Operation, p *query.Path, subjectID string) string {
	switch op {
	case query.OperationCheck:
		return "check"
	case query.OperationIterResources:
		return fmt.Sprintf("%s#%s", p.Resource.Key(), p.Relation)
	default:
		subject := p.Subject
		subject.ObjectID = subjectID
		return fmt.Sprintf("%s#%s@%s", p.Resource.Key(), p.Relation, tuple.StringONR(subject))
	}
}

// orMerge combines duplicate grants for the same cell. A nil expression means
// unconditional access, which absorbs any conditional grant.
func orMerge(a, b *core.CaveatExpression) *core.CaveatExpression {
	if a == nil || b == nil {
		return nil
	}
	return caveats.Or(a, b)
}

// subjectDomain collects the finite concrete subject IDs over which wildcard
// grants are expanded. It includes every explicitly mentioned ID, including
// excluded ones, plus the generic unmentioned-subject class ("*") for
// IterSubjects, so wildcard meaning is checked independently of how the
// executor splits wildcard and concrete paths.
func subjectDomain(op query.Operation, anchor query.ObjectAndRelation, left, right []*query.Path) []string {
	ids := map[string]struct{}{}
	var collect func(*query.Path)
	collect = func(p *query.Path) {
		if p.Subject.ObjectID != tuple.PublicWildcard {
			ids[p.Subject.ObjectID] = struct{}{}
		}
		for _, excluded := range p.ExcludedSubjects {
			collect(excluded)
		}
	}
	for _, p := range left {
		collect(p)
	}
	for _, p := range right {
		collect(p)
	}

	domain := slices.Sorted(maps.Keys(ids))
	if op == query.OperationIterSubjects {
		// The generic class represents any unmentioned subject. It must remain
		// distinct from concrete IDs: an unknown/partial wildcard never becomes
		// an unconditional grant to concrete subjects.
		domain = append(domain, tuple.PublicWildcard)
	} else {
		// Check and IterResources only resolve wildcards against the single
		// concrete anchor, which is always part of the interpretation.
		if _, ok := ids[anchor.ObjectID]; !ok {
			domain = append(domain, anchor.ObjectID)
		}
	}
	return domain
}

// buildExpressions turns a path set into a per-cell merged caveat expression
// map. Wildcard paths expand over the finite domain; excluded subjects
// subtract their caveat (or remove the grant entirely when unconditional),
// mirroring the engine's wildcard/exclusion semantics.
func buildExpressions(op query.Operation, anchor query.ObjectAndRelation, domain []string, paths []*query.Path) map[string]*core.CaveatExpression {
	exprs := map[string]*core.CaveatExpression{}
	for _, p := range paths {
		subjects := []string{p.Subject.ObjectID}
		if p.Subject.ObjectID == tuple.PublicWildcard {
			if op == query.OperationIterSubjects {
				subjects = domain
			} else {
				subjects = []string{anchor.ObjectID}
			}
		}

		for _, id := range subjects {
			expr, present := p.Caveat, true

			// Apply exclusions only when expanding a wildcard to a concrete
			// subject. The generic "*" class keeps the raw wildcard expression.
			if p.Subject.ObjectID == tuple.PublicWildcard && id != tuple.PublicWildcard {
				for _, excluded := range p.ExcludedSubjects {
					if excluded.Subject.ObjectType != p.Subject.ObjectType ||
						excluded.Subject.Relation != p.Subject.Relation ||
						excluded.Subject.ObjectID != id {
						continue
					}
					if excluded.Caveat == nil {
						// Unconditional exclusion: the concrete subject is fully
						// removed from this grant.
						present = false
						break
					}
					// Conditional exclusion subtracts its caveat condition.
					expr = caveats.And(expr, caveats.Invert(excluded.Caveat))
				}
			}
			if !present {
				continue
			}

			key := entryKey(op, p, id)
			if previous, ok := exprs[key]; ok {
				expr = orMerge(previous, expr)
			}
			exprs[key] = expr
		}
	}
	return exprs
}

// normalizeSide evaluates one side's merged expressions into classified
// cells under the supplied caveat context. Partial (unknown) caveats become
// CONDITIONAL cells and are never promoted to HAS; evaluation failures become
// per-cell CEL_ERROR entries instead of aborting the whole comparison.
func normalizeSide(ctx context.Context, schemaReader caveats.CaveatDefinitionLookup, op query.Operation, anchor query.ObjectAndRelation, domain []string, paths []*query.Path, values map[string]any) (map[string]semanticEntry, *FailureRecord) {
	exprs := buildExpressions(op, anchor, domain, paths)
	entries := make(map[string]semanticEntry, len(exprs))

	for _, key := range slices.Sorted(maps.Keys(exprs)) {
		expr := exprs[key]
		if expr == nil {
			entries[key] = semanticEntry{class: ClassHas}
			continue
		}

		result, err := caveats.RunSingleCaveatExpression(
			ctx,
			types.Default.TypeSet,
			expr,
			values,
			schemaReader,
			caveats.RunCaveatExpressionNoDebugging,
		)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, &FailureRecord{Kind: ClassCanceled, Message: err.Error()}
			}
			entries[key] = semanticEntry{class: ClassCELError, message: err.Error()}
			continue
		}

		if result.IsPartial() {
			// Unknown is a distinct, non-wildcard outcome: record the sorted
			// missing variables and the residual expression for replay.
			missing, missingErr := result.MissingVarNames()
			if missingErr != nil {
				entries[key] = semanticEntry{class: ClassCELError, message: missingErr.Error()}
				continue
			}
			slices.Sort(missing)
			entries[key] = semanticEntry{
				class:       ClassConditional,
				missingVars: missing,
				exprText:    expr.String(),
			}
			continue
		}

		if result.Value() {
			entries[key] = semanticEntry{class: ClassHas}
		} else {
			entries[key] = semanticEntry{class: ClassNo}
		}
	}
	return entries, nil
}

// entriesEqual compares two normalized cells. CONDITIONAL cells require the
// same missing variable set; CEL_ERROR cells compare on class alone (messages
// are retained in the artifact for inspection).
func entriesEqual(a, b semanticEntry) bool {
	if a.class != b.class {
		return false
	}
	if a.class == ClassConditional {
		return slices.Equal(a.missingVars, b.missingVars)
	}
	return true
}

// compareEntries returns the first mismatching cell over the joint key domain,
// materializing NO for keys only one side produces. Nil means the observable
// sets are equal: iteration order and duplicate counts never enter the
// comparison because both sides were reduced to keyed sets.
func compareEntries(left, right map[string]semanticEntry) *entryDiff {
	keys := map[string]struct{}{}
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}

	for _, key := range slices.Sorted(maps.Keys(keys)) {
		l, lok := left[key]
		if !lok {
			l = noEntry
		}
		r, rok := right[key]
		if !rok {
			r = noEntry
		}
		if !entriesEqual(l, r) {
			return &entryDiff{
				key:    key,
				left:   l,
				right:  r,
				reason: fmt.Sprintf("cell %q differs: original=%s candidate=%s", key, l.class, r.class),
			}
		}
	}
	return nil
}

// toRecord renders normalized entries as sorted, JSON-safe records, including
// explicit NO cells from the joint domain so artifacts are self-describing.
func toRecord(entries map[string]semanticEntry, jointKeys []string) []EntryRecord {
	records := make([]EntryRecord, 0, len(jointKeys))
	for _, key := range jointKeys {
		entry, ok := entries[key]
		if !ok {
			entry = noEntry
		}
		records = append(records, EntryRecord{
			Key:            key,
			Classification: entry.class,
			MissingVars:    slices.Clone(entry.missingVars),
			Expression:     entry.exprText,
			Message:        entry.message,
		})
	}
	return records
}

// jointKeys returns the sorted union of keys from both normalized maps.
func jointKeys(left, right map[string]semanticEntry) []string {
	keys := map[string]struct{}{}
	for key := range left {
		keys[key] = struct{}{}
	}
	for key := range right {
		keys[key] = struct{}{}
	}
	return slices.Sorted(maps.Keys(keys))
}
