package snapshotcompare

import (
	"fmt"

	"github.com/authzed/spicedb/pkg/datalayer"
	"github.com/authzed/spicedb/pkg/datastore"
)

// ScopeStatement is carried verbatim on every comparison footer. It states,
// for downstream consumers, that the report is bounded by the finite input
// checklist and is not a proof of equivalence over the full permission set.
const ScopeStatement = "comparison is scoped to the supplied finite checklist only; it is not a proof of full permission-set equivalence"

// Side identifies one of the two pinned snapshots.
type Side uint8

const (
	// SideUnspecified is the zero value and never appears in a populated result.
	SideUnspecified Side = iota
	// SideLeft is the baseline ("before") snapshot.
	SideLeft
	// SideRight is the candidate ("after") snapshot.
	SideRight
)

// String returns the stable lower-case name used in error locations and logs.
func (s Side) String() string {
	switch s {
	case SideLeft:
		return "left"
	case SideRight:
		return "right"
	default:
		return "unspecified"
	}
}

// SnapshotRef pins one side of a comparison to an explicit, fixed revision and
// the schema hash active at that revision. Both fields are mandatory and are
// bound into resumable cursors.
type SnapshotRef struct {
	// Revision is the fixed datastore revision read by this side.
	Revision datastore.Revision

	// SchemaHash is the schema hash visible at Revision. It is sent on every
	// Check dispatch for this side, keeping that side's schema and compiled
	// caveat cache separate from the other side's same-named definitions.
	SchemaHash datalayer.SchemaHash
}

func (r SnapshotRef) validated(name string) error {
	if r.Revision == nil || r.Revision == datastore.NoRevision {
		return fmt.Errorf("%s revision is required", name)
	}
	if r.SchemaHash == "" {
		return fmt.Errorf("%s schema hash is required", name)
	}
	return nil
}

// CheckItem is one entry of the finite checklist. Its index in the supplied
// slice is the authoritative alignment key: rows keep their original input
// position, including when the same input appears more than once.
type CheckItem struct {
	// ResourceType is the object type (namespace) of the resource.
	ResourceType string

	// Permission is the relation or permission being checked.
	Permission string

	// ResourceID is the ID of the resource.
	ResourceID string

	// SubjectType is the object type of the subject.
	SubjectType string

	// SubjectID is the ID of the subject.
	SubjectID string

	// SubjectRelation is the subject's optional relation; an empty string is
	// normalized to the ellipsis relation.
	SubjectRelation string

	// CaveatContext is the request-supplied caveat evaluation context. It is
	// shared, unchanged, by both sides for the same checklist entry.
	CaveatContext map[string]any
}

// SubjectRelationNormalized returns the subject relation with empty strings
// normalized to the datastore ellipsis.
func (i CheckItem) SubjectRelationNormalized() string {
	if i.SubjectRelation == "" {
		return datastore.Ellipsis
	}
	return i.SubjectRelation
}

// Outcome is the per-side evaluation result of one checklist entry. The five
// decision-bearing outcomes are kept distinct so that unknown or unevaluable
// inputs are never silently interpreted as an authorization or a revocation.
type Outcome uint8

const (
	// OutcomeUnspecified is the zero value and never appears in a populated
	// SideResult.
	OutcomeUnspecified Outcome = iota

	// OutcomeNoPermission means the Check completed and returned NOT_MEMBER.
	OutcomeNoPermission

	// OutcomeHasPermission means the Check completed and returned MEMBER.
	OutcomeHasPermission

	// OutcomeConditional means the Check returned a caveated member whose
	// caveat expression could not be fully evaluated from the supplied context.
	// The Condition identity and MissingContext fields describe what is known.
	OutcomeConditional

	// OutcomeInvalidInput means the input references a type, relation or
	// permission that does not exist (or is otherwise not checkable) against
	// this side's schema. No authorization decision is implied.
	OutcomeInvalidInput

	// OutcomeExecutionError means the Check path itself failed for this side
	// (for example a datastore error). No authorization decision is implied.
	OutcomeExecutionError
)

// String returns a stable lower-case name for the outcome.
func (o Outcome) String() string {
	switch o {
	case OutcomeNoPermission:
		return "no_permission"
	case OutcomeHasPermission:
		return "has_permission"
	case OutcomeConditional:
		return "conditional"
	case OutcomeInvalidInput:
		return "invalid_input"
	case OutcomeExecutionError:
		return "execution_error"
	default:
		return "unspecified"
	}
}

// ConditionIdentity is the provable identity of a conditional result on one
// side. Two conditional results are considered equal only when every field
// matches across both sides.
type ConditionIdentity struct {
	// CanonicalExpression is the order-independent, normalized rendering of the
	// caveat expression reached by the Check.
	CanonicalExpression string

	// CaveatDefinitionFingerprint hashes the exact caveat definitions
	// referenced by the expression as visible on this side's snapshot. A
	// same-named caveat with a changed CEL definition or parameters produces a
	// different fingerprint.
	CaveatDefinitionFingerprint string

	// ContextSourceHash is the stable hash of the request-supplied caveat
	// context used while evaluating the entry. The same checklist entry feeds
	// the identical context to both sides; the hash makes that provenance
	// explicit in the comparison.
	ContextSourceHash string

	// MissingContext is the sorted list of caveat context fields that would be
	// required to evaluate the condition fully.
	MissingContext []string
}

// SideError locates a failed evaluation at a specific side and original
// checklist position while retaining the underlying cause.
type SideError struct {
	// Side is the snapshot whose Check failed.
	Side Side

	// Position is the zero-based index of the entry in the original checklist.
	Position int

	// Err is the underlying failure.
	Err error
}

// Error implements error.
func (e *SideError) Error() string {
	return fmt.Sprintf("snapshot comparison check failed on %s side at checklist position %d: %s", e.Side, e.Position, e.Err)
}

// Unwrap exposes the underlying cause for errors.Is/errors.As.
func (e *SideError) Unwrap() error {
	return e.Err
}

// SideResult is one side's evaluation of one checklist entry.
type SideResult struct {
	// Side identifies which snapshot produced this result.
	Side Side

	// Outcome is the five-way evaluation outcome.
	Outcome Outcome

	// Condition is populated only when Outcome == OutcomeConditional.
	Condition *ConditionIdentity

	// InvalidReason is populated only when Outcome == OutcomeInvalidInput and
	// explains which part of the input was not valid against this schema.
	InvalidReason string

	// Failure is populated only when Outcome == OutcomeExecutionError.
	Failure *SideError
}

// ChangeKind is the matrix classification for an aligned left/right pair.
type ChangeKind uint8

const (
	// ChangeUnspecified is the zero value and never appears in a populated row.
	ChangeUnspecified ChangeKind = iota

	// ChangeAddedAccess means the left side denied (NO) an entry that the
	// right side grants (HAS).
	ChangeAddedAccess

	// ChangeRemovedAccess means the left side granted (HAS) an entry that the
	// right side denies (NO).
	ChangeRemovedAccess

	// ChangeConditionChanged means at least one side is conditional and the
	// pair is not provably identical. This includes transitions between
	// unconditional and conditional results, because an unknown caveat input
	// must not be guessed as a grant or a revocation.
	ChangeConditionChanged

	// ChangeUnchanged means both sides produced provably identical outcomes:
	// the same concrete decision (NO/NO or HAS/HAS), or conditional results
	// with identical normalized expressions, caveat definitions, context source
	// and missing context.
	ChangeUnchanged

	// ChangeIncomparable means at least one side could not produce a decision
	// (invalid input or execution failure), so the pair cannot be classified as
	// an access change or an unchanged result.
	ChangeIncomparable
)

// String returns a stable upper-camel-casename used in reports.
func (k ChangeKind) String() string {
	switch k {
	case ChangeAddedAccess:
		return "AddedAccess"
	case ChangeRemovedAccess:
		return "RemovedAccess"
	case ChangeConditionChanged:
		return "ConditionChanged"
	case ChangeUnchanged:
		return "Unchanged"
	case ChangeIncomparable:
		return "Incomparable"
	default:
		return "Unspecified"
	}
}

// MatrixRow is one aligned row of the comparison matrix. Position always
// refers back to the original checklist, even for duplicate entries.
type MatrixRow struct {
	// Position is the zero-based index of the entry in the original checklist.
	Position int

	// Item is the checklist entry evaluated for this row.
	Item CheckItem

	// Left and Right are the per-side results, aligned to the same entry.
	Left  SideResult
	Right SideResult

	// Change is the five-way matrix classification.
	Change ChangeKind
}

// Truncation reasons reported on the footer when the page is incomplete.
const (
	// TruncatedByPageSize means the configured page size was reached.
	TruncatedByPageSize = "page-size"

	// TruncatedByCheckBudget means the MaxChecks query budget was reached.
	TruncatedByCheckBudget = "max-checks-budget"

	// TruncatedByContextBudget means the MaxContextBytes budget was reached.
	TruncatedByContextBudget = "max-context-bytes-budget"

	// TruncatedByResultBudget means the MaxResultBytes budget was reached.
	TruncatedByResultBudget = "max-result-bytes-budget"
)

// Budget independently bounds the work and output of a single Compare call.
// Zero on any field means that dimension is unlimited. Budgets never cause an
// input to be evaluated differently: they only decide where a page ends.
type Budget struct {
	// MaxChecks bounds the number of per-side Check executions issued in the
	// call. Identical inputs deduplicated within the call count once per side.
	MaxChecks int

	// MaxContextBytes bounds the cumulative serialized size of the request
	// caveat contexts belonging to distinct inputs processed in the call.
	MaxContextBytes int

	// MaxResultBytes bounds the estimated serialized size of the returned page.
	// At least one row is always returned on a non-exhausted checklist so a
	// budget can never make pagination fail to progress.
	MaxResultBytes int
}

// BudgetUsage reports what the call consumed.
type BudgetUsage struct {
	// ChecksExecuted is the number of per-side Check executions actually
	// issued (deduplicated inputs are counted once per side).
	ChecksExecuted int

	// ContextBytes is the cumulative serialized caveat-context size of the
	// distinct inputs processed in the call.
	ContextBytes int

	// ResultBytes is the estimated serialized size of the rows in this page.
	ResultBytes int
}

// Footer carries the integrity and positioning information for a page.
type Footer struct {
	// Complete is true exactly when the page reaches the end of the checklist.
	// A truncated page always reports Complete == false together with a
	// TruncatedReason and a NextCursor.
	Complete bool

	// TruncatedReason is empty on a complete page, otherwise one of the
	// TruncatedBy* constants.
	TruncatedReason string

	// NextCursor resumes the comparison at the next unprocessed input
	// position. It is nil on a complete page.
	NextCursor *Cursor

	// TotalItems is the total number of entries in the checklist.
	TotalItems int

	// FirstPosition is the original checklist position of the first returned
	// row. It is zero on an empty checklist response.
	FirstPosition int

	// RowsInPage is the number of rows carried by this page.
	RowsInPage int

	// NextPosition is the position at which the following page starts, equal
	// to TotalItems when the comparison is complete.
	NextPosition int

	// Usage is the budget consumed by this call.
	Usage BudgetUsage

	// ScopeStatement always equals the package-level ScopeStatement.
	ScopeStatement string
}

// ComparisonResult is one page of the matrix plus its integrity footer.
type ComparisonResult struct {
	// Rows are ordered by original checklist position and cover a contiguous
	// position range. Duplicate inputs produce one row each.
	Rows []MatrixRow

	// Footer describes completeness, the resume cursor and budget usage.
	Footer Footer
}
