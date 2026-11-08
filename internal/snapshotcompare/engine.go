package snapshotcompare

import (
	"context"
	"errors"
	"fmt"

	"github.com/authzed/spicedb/internal/dispatch"
	"github.com/authzed/spicedb/internal/graph/computed"
	"github.com/authzed/spicedb/internal/namespace"
	"github.com/authzed/spicedb/internal/sharederrors"
	caveattypes "github.com/authzed/spicedb/pkg/caveats/types"
	"github.com/authzed/spicedb/pkg/datalayer"
	"github.com/authzed/spicedb/pkg/datastore"
	dispatchv1 "github.com/authzed/spicedb/pkg/proto/dispatch/v1"
	"github.com/authzed/spicedb/pkg/tuple"
)

const (
	// defaultMaximumDepth matches the permissions-service default API depth.
	defaultMaximumDepth uint32 = 50

	// defaultDispatchChunkSize matches the local dispatcher default chunk size.
	defaultDispatchChunkSize uint16 = 100
)

// Config configures a Comparer. The zero value is not valid; use New, which
// applies defaults to unspecified fields.
type Config struct {
	// MaximumDepth is the traversal depth budget sent on every Check. Defaults
	// to 50 when zero.
	MaximumDepth uint32

	// DispatchChunkSize is the dispatch chunk size sent on every Check.
	// Defaults to 100 when zero.
	DispatchChunkSize uint16

	// PageSize bounds the number of rows returned by a single Compare call; a
	// cursor resumes on the next position. Zero means a single unlimited page.
	PageSize int

	// Budget independently bounds check executions, caveat-context bytes and
	// estimated result size for each call. Zero fields are unlimited.
	Budget Budget
}

// Params are the fixed inputs of one comparison: two pinned snapshots and the
// finite checklist to run against both.
type Params struct {
	Left  SnapshotRef
	Right SnapshotRef
	Items []CheckItem
}

// Comparer runs PermissionSnapshotComparison pages against a datalayer and a
// Check dispatcher. It is safe for concurrent use: a Comparer holds no
// per-comparison state.
type Comparer struct {
	dl      datalayer.DataLayer
	checker dispatch.Check
	typeSet *caveattypes.TypeSet
	cfg     Config
}

// New constructs a Comparer and validates its configuration.
func New(dl datalayer.DataLayer, checker dispatch.Check, typeSet *caveattypes.TypeSet, cfg Config) (*Comparer, error) {
	if dl == nil {
		return nil, fmt.Errorf("%w: datalayer is required", ErrInvalidConfig)
	}
	if checker == nil {
		return nil, fmt.Errorf("%w: check dispatcher is required", ErrInvalidConfig)
	}
	if typeSet == nil {
		return nil, fmt.Errorf("%w: caveat type set is required", ErrInvalidConfig)
	}
	if cfg.PageSize < 0 {
		return nil, fmt.Errorf("%w: page size must be zero (unlimited) or positive", ErrInvalidConfig)
	}
	if cfg.Budget.MaxChecks < 0 || cfg.Budget.MaxContextBytes < 0 || cfg.Budget.MaxResultBytes < 0 {
		return nil, fmt.Errorf("%w: budget fields must be zero (unlimited) or positive", ErrInvalidConfig)
	}
	if cfg.MaximumDepth == 0 {
		cfg.MaximumDepth = defaultMaximumDepth
	}
	if cfg.DispatchChunkSize == 0 {
		cfg.DispatchChunkSize = defaultDispatchChunkSize
	}
	return &Comparer{dl: dl, checker: checker, typeSet: typeSet, cfg: cfg}, nil
}

// sideExecution is the position-independent outcome of evaluating one input
// on one side. It is cached per distinct input; the row-specific SideResult
// (including a failure located at that row's own position) is materialized per
// checklist position so duplicate inputs keep correct failure locations.
type sideExecution struct {
	outcome       Outcome
	condition     *ConditionIdentity
	invalidReason string
	// failure is the underlying execution error, set only when outcome is
	// OutcomeExecutionError.
	failure error
}

func (e sideExecution) toResult(side Side, position int) SideResult {
	result := SideResult{
		Side:          side,
		Outcome:       e.outcome,
		Condition:     e.condition,
		InvalidReason: e.invalidReason,
	}
	if e.failure != nil {
		result.Failure = &SideError{Side: side, Position: position, Err: e.failure}
	}
	return result
}

// executedPair caches the per-side execution facts of a distinct checklist
// input within one page. Duplicate inputs reuse it instead of issuing new
// Checks.
type executedPair struct {
	left         sideExecution
	right        sideExecution
	checksIssued int
	contextBytes int
}

// Compare executes one page of the comparison. When resume is nil the
// comparison starts at position 0; otherwise it must be a cursor previously
// returned by this comparison, bound to the same datastore, both snapshots and
// the identical checklist.
func (c *Comparer) Compare(ctx context.Context, params Params, resume *Cursor) (*ComparisonResult, error) {
	if err := params.Left.validated("left"); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if err := params.Right.validated("right"); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if params.Left.SchemaHash.IsBypassSentinel() || params.Right.SchemaHash.IsBypassSentinel() {
		return nil, fmt.Errorf("%w: schema hash bypass sentinels cannot pin a comparison side", ErrInvalidConfig)
	}
	if len(params.Items) == 0 {
		return nil, ErrEmptyChecklist
	}

	digest, err := ChecklistDigest(params.Items)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to digest checklist: %w", ErrInvalidConfig, err)
	}

	datastoreID, err := c.dl.UniqueID(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to identify datastore: %w", err)
	}

	startPosition, err := c.resolveResume(resume, datastoreID, digest, params)
	if err != nil {
		return nil, err
	}

	// Both revisions must still be servable. An expired or otherwise invalid
	// revision rejects the comparison (including every resume) before any work
	// happens.
	if err := c.checkRevisionAvailable(ctx, params.Left); err != nil {
		return nil, err
	}
	if err := c.checkRevisionAvailable(ctx, params.Right); err != nil {
		return nil, err
	}

	usage := BudgetUsage{}
	rows := make([]MatrixRow, 0, c.initialRowCapacity(len(params.Items)-startPosition))
	cache := make(map[string]executedPair)
	position := startPosition
	truncatedReason := ""

	for position < len(params.Items) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrContextCanceled, err)
		}

		item := params.Items[position]
		key, err := canonicalKey(item)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to canonicalize checklist entry: %w", ErrInvalidConfig, err)
		}

		pair, seen := cache[key]
		if !seen {
			// A fresh distinct input: enforce the context-size budget before
			// issuing any work for it.
			contextBytes, err := contextByteSize(item.CaveatContext)
			if err != nil {
				return nil, fmt.Errorf("%w: malformed caveat context at position %d: %w", ErrInvalidConfig, position, err)
			}
			if len(rows) > 0 && overBudget(c.cfg.Budget.MaxContextBytes, usage.ContextBytes+contextBytes) {
				truncatedReason = TruncatedByContextBudget
				break
			}

			// The check-query budget bounds executions, not rows. The first
			// distinct input of a page is always allowed so pagination can
			// never lose progress.
			if len(rows) > 0 && overBudget(c.cfg.Budget.MaxChecks, usage.ChecksExecuted) {
				truncatedReason = TruncatedByCheckBudget
				break
			}

			left, leftChecks, abortErr := c.runSide(ctx, SideLeft, params.Left, item)
			if abortErr != nil {
				return nil, c.abortError(abortErr)
			}
			right, rightChecks, abortErr := c.runSide(ctx, SideRight, params.Right, item)
			if abortErr != nil {
				return nil, c.abortError(abortErr)
			}

			pair = executedPair{
				left:         left,
				right:        right,
				checksIssued: leftChecks + rightChecks,
				contextBytes: contextBytes,
			}
			cache[key] = pair
			usage.ChecksExecuted += pair.checksIssued
			usage.ContextBytes += pair.contextBytes
		}

		leftResult := pair.left.toResult(SideLeft, position)
		rightResult := pair.right.toResult(SideRight, position)
		row := MatrixRow{
			Position: position,
			Item:     item,
			Left:     leftResult,
			Right:    rightResult,
			Change:   classify(leftResult, rightResult),
		}
		rowBytes := estimateRowBytes(row)

		// Again, always allow at least one row per page to guarantee progress.
		if len(rows) > 0 && overBudget(c.cfg.Budget.MaxResultBytes, usage.ResultBytes+rowBytes) {
			truncatedReason = TruncatedByResultBudget
			break
		}

		rows = append(rows, row)
		usage.ResultBytes += rowBytes
		position++

		if c.cfg.PageSize > 0 && len(rows) >= c.cfg.PageSize {
			truncatedReason = TruncatedByPageSize
			break
		}
	}

	complete := position >= len(params.Items)
	footer := Footer{
		Complete:       complete,
		TotalItems:     len(params.Items),
		FirstPosition:  startPosition,
		RowsInPage:     len(rows),
		NextPosition:   position,
		Usage:          usage,
		ScopeStatement: ScopeStatement,
	}
	if !complete {
		footer.TruncatedReason = truncatedReason
		cursor, err := EncodeCursor(datastoreID, params.Left, params.Right, digest, position)
		if err != nil {
			return nil, err
		}
		footer.NextCursor = &cursor
	}

	return &ComparisonResult{Rows: rows, Footer: footer}, nil
}

func (c *Comparer) initialRowCapacity(remaining int) int {
	if c.cfg.PageSize <= 0 || c.cfg.PageSize > remaining {
		if remaining < 0 {
			return 0
		}
		return remaining
	}
	return c.cfg.PageSize
}

func (c *Comparer) abortError(err error) error {
	if isContextAbort(err) {
		return fmt.Errorf("%w: %w", ErrContextCanceled, err)
	}
	return err
}

// checkRevisionAvailable verifies that a pinned revision is still servable and
// translates datastore invalid-revision failures into the package sentinel.
func (c *Comparer) checkRevisionAvailable(ctx context.Context, ref SnapshotRef) error {
	err := c.dl.CheckRevision(ctx, ref.Revision)
	if err == nil {
		return nil
	}
	var invalidRevision datastore.InvalidRevisionError
	if errors.As(err, &invalidRevision) {
		return fmt.Errorf("%w: %w", ErrRevisionUnavailable, err)
	}
	return fmt.Errorf("failed to verify revision %s: %w", ref.Revision, err)
}

// resolveResume validates every binding carried by a resume cursor.
func (c *Comparer) resolveResume(resume *Cursor, datastoreID, digest string, params Params) (int, error) {
	if resume == nil {
		return 0, nil
	}
	payload, err := decodeCursor(*resume)
	if err != nil {
		return 0, err
	}
	if payload.DatastoreID != datastoreID {
		return 0, fmt.Errorf("%w: cursor belongs to a different datastore", ErrCursorInvalid)
	}
	if payload.ChecklistDigest != digest {
		return 0, fmt.Errorf("%w: cursor checklist does not match the supplied checklist", ErrCursorInvalid)
	}
	if payload.LeftRevision != params.Left.Revision.String() || payload.LeftSchemaHash != string(params.Left.SchemaHash) ||
		payload.RightRevision != params.Right.Revision.String() || payload.RightSchemaHash != string(params.Right.SchemaHash) {
		return 0, fmt.Errorf("%w: cursor snapshots do not match the supplied revisions and schema hashes", ErrCursorInvalid)
	}
	if payload.NextPosition > len(params.Items) {
		return 0, fmt.Errorf("%w: cursor position %d is beyond checklist length %d",
			ErrCursorInvalid, payload.NextPosition, len(params.Items))
	}
	return payload.NextPosition, nil
}

// runSide evaluates one entry against one pinned snapshot. It returns the
// position-independent execution facts, the number of Check executions
// actually issued (0 for invalid inputs, 1 otherwise) and a non-nil error only
// for comparison-aborting failures (shared context cancellation). Per-side
// evaluation failures are captured in the returned facts so they can be
// located by side and position without aborting the rest of the matrix.
func (c *Comparer) runSide(
	ctx context.Context,
	side Side,
	ref SnapshotRef,
	item CheckItem,
) (sideExecution, int, error) {
	execution := sideExecution{}
	fail := func(err error) sideExecution {
		execution.outcome = OutcomeExecutionError
		execution.failure = err
		return execution
	}
	invalid := func(err error) sideExecution {
		execution.outcome = OutcomeInvalidInput
		execution.invalidReason = err.Error()
		return execution
	}

	reader := c.dl.SnapshotReader(ref.Revision, ref.SchemaHash)
	sr, err := reader.ReadSchema(ctx)
	if err != nil {
		if isContextAbort(err) {
			return sideExecution{}, 0, err
		}
		if isSchemaNotFoundErr(err) {
			return invalid(err), 0, nil
		}
		return fail(err), 0, nil
	}

	// Validate the input against this side's own schema. A type or permission
	// missing here is an invalid input on this side, not a denial.
	err = namespace.CheckNamespaceAndRelations(ctx, []namespace.TypeAndRelationToCheck{
		{
			NamespaceName: item.ResourceType,
			RelationName:  item.Permission,
			AllowEllipsis: false,
		},
		{
			NamespaceName: item.SubjectType,
			RelationName:  item.SubjectRelationNormalized(),
			AllowEllipsis: true,
		},
	}, sr)
	if err != nil {
		if isContextAbort(err) {
			return sideExecution{}, 0, err
		}
		if isSchemaNotFoundErr(err) {
			return invalid(err), 0, nil
		}
		return fail(err), 0, nil
	}

	checkResult, _, err := computed.ComputeCheck(
		datalayer.ContextWithDataLayer(ctx, c.dl),
		c.checker,
		c.typeSet,
		computed.CheckParameters{
			ResourceType: tuple.RelationReference{
				ObjectType: item.ResourceType,
				Relation:   item.Permission,
			},
			Subject: tuple.ObjectAndRelation{
				ObjectType: item.SubjectType,
				ObjectID:   item.SubjectID,
				Relation:   item.SubjectRelationNormalized(),
			},
			CaveatContext: item.CaveatContext,
			AtRevision:    ref.Revision,
			MaximumDepth:  c.cfg.MaximumDepth,
			DebugOption:   computed.NoDebugging,
			SchemaHash:    ref.SchemaHash,
		},
		item.ResourceID,
		c.cfg.DispatchChunkSize,
	)
	if err != nil {
		if isContextAbort(err) {
			return sideExecution{}, 0, err
		}
		return fail(err), 1, nil
	}

	switch checkResult.GetMembership() {
	case dispatchv1.ResourceCheckResult_MEMBER:
		execution.outcome = OutcomeHasPermission
	case dispatchv1.ResourceCheckResult_NOT_MEMBER:
		execution.outcome = OutcomeNoPermission
	case dispatchv1.ResourceCheckResult_CAVEATED_MEMBER:
		identity, err := buildConditionIdentity(ctx, sr, checkResult.GetExpression(),
			checkResult.GetMissingExprFields(), item.CaveatContext)
		if err != nil {
			return fail(err), 1, nil
		}
		execution.outcome = OutcomeConditional
		execution.condition = identity
	default:
		return fail(fmt.Errorf("check returned unknown membership %v", checkResult.GetMembership())), 1, nil
	}
	return execution, 1, nil
}

// classify maps two aligned per-side results onto the five-way change label.
func classify(left, right SideResult) ChangeKind {
	if left.Outcome == OutcomeInvalidInput || right.Outcome == OutcomeInvalidInput ||
		left.Outcome == OutcomeExecutionError || right.Outcome == OutcomeExecutionError {
		return ChangeIncomparable
	}

	if left.Outcome == OutcomeConditional || right.Outcome == OutcomeConditional {
		if left.Outcome == OutcomeConditional && right.Outcome == OutcomeConditional &&
			conditionsEqual(left.Condition, right.Condition) {
			return ChangeUnchanged
		}
		// Any other conditional pairing, including HAS/NO <-> CONDITIONAL
		// transitions, is a condition change: an unknown caveat input must not
		// be guessed as a grant or a revocation.
		return ChangeConditionChanged
	}

	switch {
	case left.Outcome == OutcomeNoPermission && right.Outcome == OutcomeHasPermission:
		return ChangeAddedAccess
	case left.Outcome == OutcomeHasPermission && right.Outcome == OutcomeNoPermission:
		return ChangeRemovedAccess
	case left.Outcome == right.Outcome:
		return ChangeUnchanged
	default:
		// Defensive: every concrete pairing is handled above; anything else
		// cannot be safely classified.
		return ChangeIncomparable
	}
}

// isSchemaNotFoundErr reports whether an error denotes a missing schema
// element rather than an execution-time failure. The not-found interfaces are
// implemented by both the datastore and namespace/graph error types.
func isSchemaNotFoundErr(err error) bool {
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

func isContextAbort(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
