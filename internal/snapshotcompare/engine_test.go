package snapshotcompare_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/spicedb/internal/dispatch"
	"github.com/authzed/spicedb/internal/sharederrors"
	"github.com/authzed/spicedb/internal/snapshotcompare"
	"github.com/authzed/spicedb/pkg/datastore"
	dispatchv1 "github.com/authzed/spicedb/pkg/proto/dispatch/v1"
)

func isNotFoundForTest(err error) bool {
	var unknownNamespace sharederrors.UnknownNamespaceError
	if errors.As(err, &unknownNamespace) {
		return true
	}
	var unknownRelation sharederrors.UnknownRelationError
	if errors.As(err, &unknownRelation) {
		return true
	}
	var notFound datastore.ErrNotFound
	if errors.As(err, &notFound) {
		return true
	}
	return errors.Is(err, datastore.ErrSchemaNotFound)
}

// expectedChanges is the ground-truth matrix for the fixture checklist.
var expectedChanges = []snapshotcompare.ChangeKind{
	snapshotcompare.ChangeRemovedAccess,    // 0: tom view HAS -> NO (exclusion)
	snapshotcompare.ChangeAddedAccess,      // 1: jim view NO -> HAS
	snapshotcompare.ChangeUnchanged,        // 2: becky view arrow HAS -> HAS
	snapshotcompare.ChangeUnchanged,        // 3: leo conditional via unchanged cat2
	snapshotcompare.ChangeConditionChanged, // 4: mia conditional via redefined cat1
	snapshotcompare.ChangeConditionChanged, // 5: mia HAS -> CONDITIONAL under new cat1
	snapshotcompare.ChangeUnchanged,        // 6: tom manage HAS -> HAS
	snapshotcompare.ChangeIncomparable,     // 7: admin only exists on the right
	snapshotcompare.ChangeIncomparable,     // 8: unknown type on both sides
	snapshotcompare.ChangeRemovedAccess,    // 9: duplicate of position 0
	snapshotcompare.ChangeUnchanged,        // 10: nia NO -> NO
}

func TestCompareMatrixSemanticsAndDirectCrossCheck(t *testing.T) {
	t.Parallel()
	f := newComparisonFixture(t)
	ctx := t.Context()

	result, err := f.newComparer(snapshotcompare.Config{}).Compare(ctx, f.params(), nil)
	require.NoError(t, err)

	// Footer: complete single page, explicit finite-scope notice.
	require.True(t, result.Footer.Complete)
	require.Empty(t, result.Footer.TruncatedReason)
	require.Nil(t, result.Footer.NextCursor)
	require.Equal(t, len(f.items), result.Footer.RowsInPage)
	require.Equal(t, len(f.items), result.Footer.NextPosition)
	require.Equal(t, snapshotcompare.ScopeStatement, result.Footer.ScopeStatement)

	require.Len(t, result.Rows, len(f.items))
	for pos, row := range result.Rows {
		require.Equal(t, pos, row.Position, "rows must align to original input positions")
		require.Equal(t, expectedChanges[pos], row.Change, "position %d", pos)

		// Every row must agree with a Check issued independently of the engine
		// on each side (invalid inputs validated independently as well).
		require.Equal(t, f.directCheck(ctx, snapshotcompare.SideLeft, f.left, row.Item),
			row.Left.Outcome, "position %d left side", pos)
		require.Equal(t, f.directCheck(ctx, snapshotcompare.SideRight, f.right, row.Item),
			row.Right.Outcome, "position %d right side", pos)
	}

	// Spot-check the five outcomes are represented distinctly.
	outcomeKinds := map[snapshotcompare.Outcome]struct{}{}
	for _, row := range result.Rows {
		outcomeKinds[row.Left.Outcome] = struct{}{}
		outcomeKinds[row.Right.Outcome] = struct{}{}
	}
	for _, expected := range []snapshotcompare.Outcome{
		snapshotcompare.OutcomeNoPermission,
		snapshotcompare.OutcomeHasPermission,
		snapshotcompare.OutcomeConditional,
		snapshotcompare.OutcomeInvalidInput,
	} {
		require.Contains(t, outcomeKinds, expected)
	}

	// Row 0: concrete removal with located side results.
	row0 := result.Rows[0]
	require.Equal(t, snapshotcompare.OutcomeHasPermission, row0.Left.Outcome)
	require.Equal(t, snapshotcompare.OutcomeNoPermission, row0.Right.Outcome)
	require.Nil(t, row0.Left.Failure)
	require.Nil(t, row0.Right.Failure)

	// Row 3: conditional identity is provably the same via the unchanged cat2.
	row3 := result.Rows[3]
	require.Equal(t, snapshotcompare.OutcomeConditional, row3.Left.Outcome)
	require.Equal(t, snapshotcompare.OutcomeConditional, row3.Right.Outcome)
	require.NotNil(t, row3.Left.Condition)
	require.NotNil(t, row3.Right.Condition)
	require.Equal(t, row3.Left.Condition.CanonicalExpression, row3.Right.Condition.CanonicalExpression)
	require.Equal(t, row3.Left.Condition.CaveatDefinitionFingerprint, row3.Right.Condition.CaveatDefinitionFingerprint)
	require.Equal(t, row3.Left.Condition.ContextSourceHash, row3.Right.Condition.ContextSourceHash)
	require.Equal(t, []string{"allowed"}, row3.Left.Condition.MissingContext)

	// Row 4: same caveat name but a changed definition must not look unchanged.
	row4 := result.Rows[4]
	require.Equal(t, row4.Left.Condition.CanonicalExpression, row4.Right.Condition.CanonicalExpression)
	require.NotEqual(t, row4.Left.Condition.CaveatDefinitionFingerprint, row4.Right.Condition.CaveatDefinitionFingerprint)
	require.Equal(t, []string{"condition"}, row4.Left.Condition.MissingContext)
	require.Equal(t, []string{"condition", "extra"}, row4.Right.Condition.MissingContext)

	// Row 5: an unknown caveat input on the right is never inferred as a
	// revocation; it stays conditional and is labeled ConditionChanged.
	row5 := result.Rows[5]
	require.Equal(t, snapshotcompare.OutcomeHasPermission, row5.Left.Outcome)
	require.Equal(t, snapshotcompare.OutcomeConditional, row5.Right.Outcome)
	require.Equal(t, []string{"extra"}, row5.Right.Condition.MissingContext)
	require.NotEmpty(t, row5.Right.Condition.ContextSourceHash)

	// Row 7: permission missing on the left is invalid input, not a denial.
	row7 := result.Rows[7]
	require.Equal(t, snapshotcompare.OutcomeInvalidInput, row7.Left.Outcome)
	require.NotEmpty(t, row7.Left.InvalidReason)
	require.Equal(t, snapshotcompare.OutcomeHasPermission, row7.Right.Outcome)

	// Row 8: invalid on both sides remains incomparable, never guessed.
	row8 := result.Rows[8]
	require.Equal(t, snapshotcompare.OutcomeInvalidInput, row8.Left.Outcome)
	require.Equal(t, snapshotcompare.OutcomeInvalidInput, row8.Right.Outcome)

	// Dedup: 11 entries but only 10 distinct inputs; the widget entry issues no
	// checks and the admin entry issues one (right side only).
	// 8 fully-evaluated distinct inputs * 2 sides + 1 = 17 executions.
	require.Equal(t, 17, result.Footer.Usage.ChecksExecuted)
}

func TestPaginationMatchesSingleFullMatrix(t *testing.T) {
	t.Parallel()
	f := newComparisonFixture(t)
	ctx := t.Context()

	full, err := f.newComparer(snapshotcompare.Config{}).Compare(ctx, f.params(), nil)
	require.NoError(t, err)
	require.True(t, full.Footer.Complete)

	for _, pageSize := range []int{1, 2, 3, 7} {
		t.Run("page-size", func(t *testing.T) {
			comparer := f.newComparer(snapshotcompare.Config{PageSize: pageSize})
			assembled := make([]snapshotcompare.MatrixRow, 0, len(f.items))
			var cursor *snapshotcompare.Cursor
			pages := 0
			for {
				page, err := comparer.Compare(ctx, f.params(), cursor)
				require.NoError(t, err)
				pages++
				require.Equal(t, snapshotcompare.ScopeStatement, page.Footer.ScopeStatement)

				if !page.Footer.Complete {
					require.NotNil(t, page.Footer.NextCursor)
					require.Equal(t, snapshotcompare.TruncatedByPageSize, page.Footer.TruncatedReason)
					require.LessOrEqual(t, page.Footer.RowsInPage, pageSize)
					cursor = page.Footer.NextCursor
				} else {
					require.Nil(t, page.Footer.NextCursor)
					require.Empty(t, page.Footer.TruncatedReason)
				}

				assembled = append(assembled, page.Rows...)
				if page.Footer.Complete {
					break
				}
			}
			require.GreaterOrEqual(t, pages, 2)
			require.Len(t, assembled, len(full.Rows))
			require.True(t, reflect.DeepEqual(full.Rows, assembled),
				"paginated matrix must be identical to the full matrix")
		})
	}
}

func TestCursorRejections(t *testing.T) {
	t.Parallel()
	f := newComparisonFixture(t)
	ctx := t.Context()
	digest, err := snapshotcompare.ChecklistDigest(f.items)
	require.NoError(t, err)

	paged, err := f.newComparer(snapshotcompare.Config{PageSize: 2}).Compare(ctx, f.params(), nil)
	require.NoError(t, err)
	require.False(t, paged.Footer.Complete)
	validCursor := paged.Footer.NextCursor

	t.Run("garbage token", func(t *testing.T) {
		bad := snapshotcompare.Cursor("this-is-not-a-cursor###")
		_, err := f.newComparer(snapshotcompare.Config{}).Compare(ctx, f.params(), &bad)
		require.ErrorIs(t, err, snapshotcompare.ErrCursorInvalid)
	})

	t.Run("different checklist digest", func(t *testing.T) {
		shorter := f.params()
		shorter.Items = shorter.Items[:len(shorter.Items)-1]
		_, err := f.newComparer(snapshotcompare.Config{}).Compare(ctx, shorter, validCursor)
		require.ErrorIs(t, err, snapshotcompare.ErrCursorInvalid)
	})

	t.Run("different snapshot binding", func(t *testing.T) {
		// The cursor records a different right schema hash than the params now
		// carry: a cursor can only resume the exact snapshots it was bound to.
		tampered, err := snapshotcompare.EncodeCursor(f.datastoreID, f.left,
			snapshotcompare.SnapshotRef{Revision: f.right.Revision, SchemaHash: "tampered-schema-hash"},
			digest, 0)
		require.NoError(t, err)
		_, err = f.newComparer(snapshotcompare.Config{}).Compare(ctx, f.params(), &tampered)
		require.ErrorIs(t, err, snapshotcompare.ErrCursorInvalid)
	})

	t.Run("position beyond checklist", func(t *testing.T) {
		tampered, err := snapshotcompare.EncodeCursor(f.datastoreID, f.left, f.right, digest, len(f.items)+1)
		require.NoError(t, err)
		_, err = f.newComparer(snapshotcompare.Config{}).Compare(ctx, f.params(), &tampered)
		require.ErrorIs(t, err, snapshotcompare.ErrCursorInvalid)
	})

	t.Run("expired revision on resume", func(t *testing.T) {
		future, err := f.dl.RevisionFromString("9223372036854775807")
		require.NoError(t, err)
		futureLeft := snapshotcompare.SnapshotRef{Revision: future, SchemaHash: f.left.SchemaHash}
		expired, err := snapshotcompare.EncodeCursor(f.datastoreID, futureLeft, f.right, digest, 0)
		require.NoError(t, err)
		params := f.params()
		params.Left = futureLeft
		_, err = f.newComparer(snapshotcompare.Config{}).Compare(ctx, params, &expired)
		require.ErrorIs(t, err, snapshotcompare.ErrRevisionUnavailable)
		var invalidRevision datastore.InvalidRevisionError
		require.True(t, errors.As(err, &invalidRevision))
	})

	t.Run("expired revision on fresh start", func(t *testing.T) {
		future, err := f.dl.RevisionFromString("9223372036854775807")
		require.NoError(t, err)
		params := f.params()
		params.Left = snapshotcompare.SnapshotRef{Revision: future, SchemaHash: f.left.SchemaHash}
		_, err = f.newComparer(snapshotcompare.Config{}).Compare(ctx, params, nil)
		require.ErrorIs(t, err, snapshotcompare.ErrRevisionUnavailable)
	})

	t.Run("resume at end returns complete empty page", func(t *testing.T) {
		endCursor, err := snapshotcompare.EncodeCursor(f.datastoreID, f.left, f.right, digest, len(f.items))
		require.NoError(t, err)
		page, err := f.newComparer(snapshotcompare.Config{}).Compare(ctx, f.params(), &endCursor)
		require.NoError(t, err)
		require.True(t, page.Footer.Complete)
		require.Empty(t, page.Rows)
		require.Equal(t, len(f.items), page.Footer.NextPosition)
	})
}

func TestBudgets(t *testing.T) {
	t.Parallel()
	f := newComparisonFixture(t)
	ctx := t.Context()

	full, err := f.newComparer(snapshotcompare.Config{}).Compare(ctx, f.params(), nil)
	require.NoError(t, err)

	t.Run("check budget truncates and resumes to the same matrix", func(t *testing.T) {
		comparer := f.newComparer(snapshotcompare.Config{Budget: snapshotcompare.Budget{MaxChecks: 2}})
		first, err := comparer.Compare(ctx, f.params(), nil)
		require.NoError(t, err)
		require.False(t, first.Footer.Complete)
		require.Equal(t, snapshotcompare.TruncatedByCheckBudget, first.Footer.TruncatedReason)
		require.Equal(t, 2, first.Footer.Usage.ChecksExecuted)
		require.Len(t, first.Rows, 1)

		assembled := append([]snapshotcompare.MatrixRow{}, first.Rows...)
		cursor := first.Footer.NextCursor
		for {
			page, err := comparer.Compare(ctx, f.params(), cursor)
			require.NoError(t, err)
			assembled = append(assembled, page.Rows...)
			if page.Footer.Complete {
				break
			}
			cursor = page.Footer.NextCursor
		}
		require.True(t, reflect.DeepEqual(full.Rows, assembled))
	})

	t.Run("context byte budget stops before the context-bearing input", func(t *testing.T) {
		// Position 5 is the first entry carrying request caveat context.
		comparer := f.newComparer(snapshotcompare.Config{Budget: snapshotcompare.Budget{MaxContextBytes: 1}})
		page, err := comparer.Compare(ctx, f.params(), nil)
		require.NoError(t, err)
		require.False(t, page.Footer.Complete)
		require.Equal(t, snapshotcompare.TruncatedByContextBudget, page.Footer.TruncatedReason)
		require.Len(t, page.Rows, 5)
		require.Equal(t, 5, page.Footer.NextPosition, "position 5 is the first context-bearing input")
	})

	t.Run("result size budget always makes progress", func(t *testing.T) {
		comparer := f.newComparer(snapshotcompare.Config{Budget: snapshotcompare.Budget{MaxResultBytes: 1}})
		rows := 0
		var cursor *snapshotcompare.Cursor
		for {
			page, err := comparer.Compare(ctx, f.params(), cursor)
			require.NoError(t, err)
			if !page.Footer.Complete {
				require.Equal(t, snapshotcompare.TruncatedByResultBudget, page.Footer.TruncatedReason)
				require.Len(t, page.Rows, 1, "a one-byte budget still returns at least one row")
				cursor = page.Footer.NextCursor
			}
			rows += len(page.Rows)
			if page.Footer.Complete {
				break
			}
		}
		require.Equal(t, len(f.items), rows)
	})
}

func TestSharedCancellation(t *testing.T) {
	t.Parallel()
	f := newComparisonFixture(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.newComparer(snapshotcompare.Config{}).Compare(ctx, f.params(), nil)
	require.ErrorIs(t, err, snapshotcompare.ErrContextCanceled)
	require.ErrorIs(t, err, context.Canceled)
}

// sideFailingCheck wraps a dispatcher's Check interface and fails every check
// dispatched for one schema hash, i.e. one comparison side.
type sideFailingCheck struct {
	dispatch.Check
	failSchemaHash string
}

func (f *sideFailingCheck) DispatchCheck(ctx context.Context, req *dispatchv1.DispatchCheckRequest) (*dispatchv1.DispatchCheckResponse, error) {
	if string(req.GetMetadata().GetSchemaHash()) == f.failSchemaHash {
		// Real dispatchers always return a response carrying metadata, even on
		// error; mirror that contract so the compute path does not nil-panic.
		return &dispatchv1.DispatchCheckResponse{
			Metadata: &dispatchv1.ResponseMeta{DispatchCount: 0},
		}, errors.New("injected side failure")
	}
	return f.Check.DispatchCheck(ctx, req)
}

func TestFailureLocatedToSideAndIndex(t *testing.T) {
	t.Parallel()
	f := newComparisonFixture(t)
	ctx := t.Context()

	comparer, err := snapshotcompare.New(f.dl,
		&sideFailingCheck{Check: f.dispatcher, failSchemaHash: string(f.right.SchemaHash)},
		caveatTypes(),
		snapshotcompare.Config{})
	require.NoError(t, err)

	result, err := comparer.Compare(ctx, f.params(), nil)
	require.NoError(t, err, "per-side failures are captured, not raised as call errors")

	row0 := result.Rows[0]
	require.Equal(t, snapshotcompare.OutcomeHasPermission, row0.Left.Outcome)
	require.Equal(t, snapshotcompare.OutcomeExecutionError, row0.Right.Outcome)
	require.NotNil(t, row0.Right.Failure)
	require.Equal(t, snapshotcompare.SideRight, row0.Right.Failure.Side)
	require.Equal(t, 0, row0.Right.Failure.Position)
	require.ErrorContains(t, row0.Right.Failure, "injected side failure")
	require.Equal(t, snapshotcompare.ChangeIncomparable, row0.Change)

	// Invalid inputs never reach the dispatcher, so they stay invalid even
	// while the side's Check path is failing.
	row8 := result.Rows[8]
	require.Equal(t, snapshotcompare.OutcomeInvalidInput, row8.Left.Outcome)
	require.Equal(t, snapshotcompare.OutcomeInvalidInput, row8.Right.Outcome)

	// The duplicate at position 9 reuses the cached execution facts but its
	// failure location must point at its own row position, not the first one.
	row9 := result.Rows[9]
	require.Equal(t, snapshotcompare.OutcomeExecutionError, row9.Right.Outcome)
	require.NotNil(t, row9.Right.Failure)
	require.Equal(t, snapshotcompare.SideRight, row9.Right.Failure.Side)
	require.Equal(t, 9, row9.Right.Failure.Position)
}

// recordingCheck records the (revision, schema hash) pair carried by every
// dispatched Check, proving the engine pins each side to its own snapshot
// rather than reusing the other side's schema/cache key.
type recordingCheck struct {
	dispatch.Check
	mu      sync.Mutex
	records []revisionHash
}

type revisionHash struct {
	revision   string
	schemaHash string
}

func (r *recordingCheck) DispatchCheck(ctx context.Context, req *dispatchv1.DispatchCheckRequest) (*dispatchv1.DispatchCheckResponse, error) {
	r.mu.Lock()
	r.records = append(r.records, revisionHash{
		revision:   req.GetMetadata().GetAtRevision(),
		schemaHash: string(req.GetMetadata().GetSchemaHash()),
	})
	r.mu.Unlock()
	return r.Check.DispatchCheck(ctx, req)
}

func TestEachSideDispatchesItsOwnRevisionAndSchemaHash(t *testing.T) {
	t.Parallel()
	f := newComparisonFixture(t)
	ctx := t.Context()

	recorder := &recordingCheck{Check: f.dispatcher}
	comparer, err := snapshotcompare.New(f.dl, recorder, caveatTypes(), snapshotcompare.Config{})
	require.NoError(t, err)

	result, err := comparer.Compare(ctx, f.params(), nil)
	require.NoError(t, err)

	leftPair := revisionHash{revision: f.left.Revision.String(), schemaHash: string(f.left.SchemaHash)}
	rightPair := revisionHash{revision: f.right.Revision.String(), schemaHash: string(f.right.SchemaHash)}
	seenLeft, seenRight := false, false
	for _, record := range recorder.records {
		// Every dispatch must be bound to exactly one of the two pinned
		// snapshots: no empty hash, no sentinel, no cross-side mixing.
		if record == leftPair {
			seenLeft = true
			continue
		}
		if record == rightPair {
			seenRight = true
			continue
		}
		t.Fatalf("dispatch carried unexpected snapshot binding %+v", record)
	}
	require.True(t, seenLeft, "expected at least one left-snapshot dispatch")
	require.True(t, seenRight, "expected at least one right-snapshot dispatch")
	// Nested dispatches (e.g. arrow expansion) carry the same pinned metadata,
	// so at least one dispatch exists per issued top-level Check.
	require.GreaterOrEqual(t, len(recorder.records), result.Footer.Usage.ChecksExecuted)
}

func TestComparisonIsReadOnly(t *testing.T) {
	t.Parallel()
	f := newComparisonFixture(t)
	ctx := t.Context()

	before, _, err := f.dl.HeadRevision(ctx)
	require.NoError(t, err)

	_, err = f.newComparer(snapshotcompare.Config{PageSize: 3}).Compare(ctx, f.params(), nil)
	require.NoError(t, err)

	after, _, err := f.dl.HeadRevision(ctx)
	require.NoError(t, err)
	require.True(t, after.Equal(before), "comparison must not create revisions or mutate data")
}

func TestChecklistDigest(t *testing.T) {
	t.Parallel()
	f := newComparisonFixture(t)

	digest1, err := snapshotcompare.ChecklistDigest(f.items)
	require.NoError(t, err)
	require.Len(t, digest1, 64)

	digest2, err := snapshotcompare.ChecklistDigest(f.items)
	require.NoError(t, err)
	require.Equal(t, digest1, digest2)

	changed := append([]snapshotcompare.CheckItem{}, f.items...)
	changed[0].ResourceID = "different"
	digest3, err := snapshotcompare.ChecklistDigest(changed)
	require.NoError(t, err)
	require.NotEqual(t, digest1, digest3)
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	f := newComparisonFixture(t)

	_, err := snapshotcompare.New(f.dl, f.dispatcher, caveatTypes(), snapshotcompare.Config{PageSize: -1})
	require.ErrorIs(t, err, snapshotcompare.ErrInvalidConfig)

	_, err = f.newComparer(snapshotcompare.Config{}).Compare(t.Context(),
		snapshotcompare.Params{Left: f.left, Right: f.right}, nil)
	require.ErrorIs(t, err, snapshotcompare.ErrEmptyChecklist)
}
