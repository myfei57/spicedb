package planequiv

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/authzed/spicedb/pkg/query"
	"github.com/authzed/spicedb/pkg/tuple"
)

func pathFor(subjectID, relation string) *query.Path {
	return &query.Path{
		Resource: query.NewObject("document", "o_a"),
		Relation: "view",
		Subject:  query.NewObject("user", subjectID).WithRelation(relation),
	}
}

// TestCompareEntriesIsSetBased proves order and duplicate counts never decide
// equivalence: the same paths in different orders, or repeated, normalize to
// the same keyed set.
func TestCompareEntriesIsSetBased(t *testing.T) {
	a, b := pathFor("o_a", tuple.Ellipsis), pathFor("o_b", tuple.Ellipsis)
	op := query.OperationIterSubjects
	req := Request{Operation: op, Subject: query.NewObject("user", "o_a").WithEllipses()}

	first, firstFailure := normalizeSide(t.Context(), nil, op, req.Subject, subjectDomain(op, req.Subject, []*query.Path{a, b}, nil), []*query.Path{a, b}, nil)
	require.Nil(t, firstFailure)
	second, secondFailure := normalizeSide(t.Context(), nil, op, req.Subject, subjectDomain(op, req.Subject, nil, []*query.Path{b, a, a}), []*query.Path{b, a, a}, nil)
	require.Nil(t, secondFailure)
	require.Nil(t, compareEntries(first, second))

	// A different subject relation is a distinct cell.
	other := pathFor("o_a", "member")
	third, thirdFailure := normalizeSide(t.Context(), nil, op, req.Subject, subjectDomain(op, req.Subject, nil, []*query.Path{other}), []*query.Path{other}, nil)
	require.Nil(t, thirdFailure)
	require.NotNil(t, compareEntries(first, third))
}

// TestNoIsMaterializedForMissingKeys ensures an absent grant surfaces as an
// explicit NO cell on the joint domain instead of silently passing.
func TestNoIsMaterializedForMissingKeys(t *testing.T) {
	a := pathFor("o_a", tuple.Ellipsis)
	op := query.OperationIterSubjects
	req := Request{Operation: op, Subject: query.NewObject("user", "o_a").WithEllipses()}
	domain := subjectDomain(op, req.Subject, []*query.Path{a}, nil)

	left, leftFailure := normalizeSide(t.Context(), nil, op, req.Subject, domain, []*query.Path{a}, nil)
	require.Nil(t, leftFailure)
	right, rightFailure := normalizeSide(t.Context(), nil, op, req.Subject, domain, nil, nil)
	require.Nil(t, rightFailure)

	diff := compareEntries(left, right)
	require.NotNil(t, diff)
	require.Equal(t, ClassHas, diff.left.class)
	require.Equal(t, ClassNo, diff.right.class)
}

// TestUnknownNeverActsAsWildcardValue pins the distinct treatment of partial
// caveat outcomes: CONDITIONAL never equals HAS/NO, and two residuals with
// different missing variable sets never compare equal.
func TestUnknownNeverActsAsWildcardValue(t *testing.T) {
	conditionalX := semanticEntry{class: ClassConditional, missingVars: []string{"x"}}
	conditionalY := semanticEntry{class: ClassConditional, missingVars: []string{"y"}}
	conditionalXAgain := semanticEntry{class: ClassConditional, missingVars: []string{"x"}}

	require.False(t, entriesEqual(conditionalX, semanticEntry{class: ClassHas}))
	require.False(t, entriesEqual(conditionalX, noEntry))
	require.False(t, entriesEqual(conditionalX, conditionalY))
	require.True(t, entriesEqual(conditionalX, conditionalXAgain))
	require.True(t, entriesEqual(noEntry, semanticEntry{class: ClassNo}))
}

// TestDrainStopsAndReleasesOnCancel verifies cancellation promptly terminates
// a blocked iterator: the range-over-func cleanup runs when the yield stops and
// the classified failure is CANCELED rather than a wrong empty result.
func TestDrainStopsAndReleasesOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cleanedUp := make(chan struct{})
	seq := query.PathSeq(func(yield func(*query.Path, error) bool) {
		defer close(cleanedUp)
		<-ctx.Done()
		yield(nil, ctx.Err())
	})

	go func() {
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()

	paths, failure := drain(ctx, seq)
	require.NotNil(t, failure)
	require.Equal(t, ClassCanceled, failure.Kind)
	require.Empty(t, paths)

	select {
	case <-cleanedUp:
	case <-time.After(2 * time.Second):
		t.Fatal("iterator cleanup did not run after cancellation")
	}
}

func TestPartitionAndRangeRemoval(t *testing.T) {
	relationships := make([]tuple.Relationship, 0, 3)
	for _, text := range []string{
		"document:o_a#viewer@user:o_a",
		"document:o_b#viewer@user:o_b",
		"document:o_c#viewer@user:o_c",
	} {
		relationships = append(relationships, tuple.MustParse(text))
	}
	chunks := partition(len(relationships), 2)
	require.Equal(t, []chunkRange{{0, 2}, {2, 3}}, chunks)

	reduced := withoutRange(relationships, chunks[0].start, chunks[0].end)
	require.Len(t, reduced, 1)
	require.Equal(t, "o_c", reduced[0].Resource.ObjectID)
}

func TestClassifyFailureSeparatesClasses(t *testing.T) {
	require.Equal(t, ClassBudgetExhausted, classifyFailure(query.MaxRecursionDepthError{Depth: 1}).Kind)
	require.Equal(t, ClassCanceled, classifyFailure(context.Canceled).Kind)
	require.Equal(t, ClassCanceled, classifyFailure(context.DeadlineExceeded).Kind)
	require.Equal(t, ClassExecutionError, classifyFailure(assertionError{}).Kind)
	require.Nil(t, classifyFailure(nil))
}

type assertionError struct{}

func (assertionError) Error() string { return "boom" }
