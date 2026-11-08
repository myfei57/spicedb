package planequiv

import (
	"context"
	"fmt"

	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/query"
	"github.com/authzed/spicedb/pkg/schema/v2"
	"github.com/authzed/spicedb/pkg/tuple"
)

// Classification is the observable semantic category of one normalized
// result entry or one whole-side execution failure. Categories are kept
// deliberately distinct: in particular a CONDITIONAL (partial caveat) result
// is never collapsed into HAS, and an error class never stands in for a
// membership answer.
type Classification string

const (
	// ClassHas means unconditional membership: no caveat, or a caveat that
	// evaluated to true under the supplied context.
	ClassHas Classification = "HAS"

	// ClassNo means explicit non-membership for a key in the joint comparison
	// domain. It is materialized for keys that only the other side produces so
	// that differences are visible rather than implicit through absence.
	ClassNo Classification = "NO"

	// ClassConditional means membership depends on caveat variables missing
	// from the context. The sorted missing variable names and the residual
	// expression are part of the entry, so two different residual unknowns do
	// not compare equal.
	ClassConditional Classification = "CONDITIONAL"

	// ClassCELError means a caveat expression failed to evaluate (for example a
	// CEL evaluation error or a parameter type error). It is never treated as
	// either allow or deny.
	ClassCELError Classification = "CEL_ERROR"

	// ClassBudgetExhausted means the query engine's recursion depth budget was
	// exhausted (query.MaxRecursionDepthError). The answer is unknown, not a
	// denial, and it must not be compared as an empty result set.
	ClassBudgetExhausted Classification = "BUDGET_EXHAUSTED"

	// ClassCompileError means a caller-supplied plan could not be compiled.
	ClassCompileError Classification = "COMPILE_ERROR"

	// ClassExecutionError covers executor failures that are neither CEL nor
	// budget failures.
	ClassExecutionError Classification = "EXECUTION_ERROR"

	// ClassCanceled means execution stopped because the validator's context
	// was canceled or its deadline passed. Cancellation is propagated to the
	// caller rather than reported as a semantic divergence.
	ClassCanceled Classification = "CANCELED"
)

// FiniteModel is one explicit finite interpretation of the schema: a fixed
// relationship set and the caveat contexts to evaluate requests under.
// Relationships are loaded into a fresh in-process memdb snapshot; nothing
// here ever reaches an external datastore.
type FiniteModel struct {
	// Name is an optional label used in diagnostics and artifacts.
	Name string

	// Relationships is the complete finite relationship set for this model.
	Relationships []tuple.Relationship

	// CaveatContexts lists the caveat evaluation contexts to exercise. An
	// empty slice is treated as a single nil (empty) context. Every request is
	// replayed once per context.
	CaveatContexts []map[string]any
}

// Request is one concrete operation to execute under every model and context.
type Request struct {
	// Operation selects Check, IterResources or IterSubjects.
	Operation query.Operation

	// Resource is the resource anchor for Check and IterSubjects.
	Resource query.Object

	// Subject is the subject anchor for Check and IterResources.
	Subject query.ObjectAndRelation

	// SubjectFilter is the filter type/relation for IterSubjects. Ignored for
	// the other operations.
	SubjectFilter query.ObjectType
}

// Input is the complete, caller-supplied validation workload. The validator
// takes both plans as given and never runs either side through an optimizer;
// the candidate plan is therefore fully under the caller's control.
type Input struct {
	// Schema is the fixed schema both plans were built from.
	Schema *schema.Schema

	// CaveatDefinitions retains the compiled caveat definitions with their
	// parameter types, which Schema.ToDefinitions omits. Required whenever the
	// schema references caveats.
	CaveatDefinitions []*core.CaveatDefinition

	// SchemaText optionally records the original schema DSL for replayable
	// artifacts. When empty the artifact records the compiled definitions
	// instead.
	SchemaText string

	// Original is the reference plan.
	Original query.CanonicalOutline

	// Candidate is the plan under test. It is compiled and executed
	// independently; the validator never rewrites, canonicalizes or optimizes
	// it.
	Candidate query.CanonicalOutline

	// Models is the explicit finite interpretation list. Must be non-empty.
	Models []FiniteModel

	// Requests is the explicit finite request list. Must be non-empty.
	Requests []Request
}

// Option configures a PlanEquivalenceValidator. Options can only tighten the
// built-in default limits, never exceed the hard ceilings.
type Option func(*config) error

type config struct {
	limits         limits
	deltaDebug     bool
	deltaReruns    int
	recursionDepth int
}

// WithMaxModels caps the number of finite models accepted per Input.
func WithMaxModels(n int) Option {
	return func(c *config) error {
		return boundedOption(&c.limits.maxModels, hardMaxModels, "max models", n)
	}
}

// WithMaxRequests caps the number of requests accepted per Input.
func WithMaxRequests(n int) Option {
	return func(c *config) error {
		return boundedOption(&c.limits.maxRequests, hardMaxRequests, "max requests", n)
	}
}

// WithMaxRelationships caps the relationship count in any single model.
func WithMaxRelationships(n int) Option {
	return func(c *config) error {
		return boundedOption(&c.limits.maxRelationships, hardMaxRelationships, "max relationships", n)
	}
}

// WithMaxPlanDepth caps the accepted outline tree depth of either plan.
func WithMaxPlanDepth(n int) Option {
	return func(c *config) error {
		return boundedOption(&c.limits.maxPlanDepth, hardMaxPlanDepth, "max plan depth", n)
	}
}

// WithMaxPlanNodes caps the total outline node count of either plan.
func WithMaxPlanNodes(n int) Option {
	return func(c *config) error {
		return boundedOption(&c.limits.maxPlanNodes, hardMaxPlanNodes, "max plan nodes", n)
	}
}

// WithMaxArtifactBytes caps the serialized size of a counterexample artifact.
// Artifacts that would exceed the cap are rejected rather than truncated.
func WithMaxArtifactBytes(n int) Option {
	return func(c *config) error {
		return boundedOption(&c.limits.maxArtifactBytes, hardMaxArtifactBytes, "max artifact bytes", n)
	}
}

// WithRecursionDepth sets the per-execution recursion depth budget. It
// controls when BUDGET_EXHAUSTED is observed.
func WithRecursionDepth(depth int) Option {
	return func(c *config) error {
		if err := boundedOption(&c.limits.maxRecursionDepth, hardMaxRecursionDepth, "recursion depth", depth); err != nil {
			return err
		}
		c.recursionDepth = depth
		return nil
	}
}

// WithDeltaDebugging enables optional counterexample reduction. Reduction only
// deletes relationships and top-level caveat context fields, rerunning the
// real executor after each deletion while the semantic difference survives.
// maxReruns bounds the number of paired executions; zero disables reduction.
// The reduced input is not claimed to be a global minimum.
func WithDeltaDebugging(maxReruns int) Option {
	return func(c *config) error {
		if maxReruns < 0 || maxReruns > hardMaxDeltaDebugReruns {
			return fmt.Errorf("planequiv: delta debugging reruns must be between 0 and %d, got %d", hardMaxDeltaDebugReruns, maxReruns)
		}
		c.deltaDebug = maxReruns > 0
		c.deltaReruns = maxReruns
		return nil
	}
}

// Report is the validator's verdict.
type Report struct {
	// Equivalent is true when every (model, request, context) triple produced
	// matching observable semantics.
	Equivalent bool

	// ChecksEvaluated is the number of triples compared before the report
	// stopped. A counterexample, if any, is the first divergent triple.
	ChecksEvaluated int

	// Counterexample is populated when Equivalent is false.
	Counterexample *Counterexample
}

// FailureRecord describes a whole-side execution or compilation failure. It
// is data-only and safe to serialize.
type FailureRecord struct {
	// Kind is the classified failure category.
	Kind Classification `json:"kind"`

	// Message is the executor's error text. It is retained for diagnostics;
	// equivalence comparisons use Kind only.
	Message string `json:"message"`
}

// EntryRecord is one normalized, order-independent result entry in a side's
// observable output.
type EntryRecord struct {
	// Key identifies the normalized membership cell. For lookups it is the
	// resource#relation@subject tuple (resource#relation for IterResources);
	// Check uses the fixed key "check".
	Key string `json:"key"`

	// Classification is the entry's semantic category.
	Classification Classification `json:"classification"`

	// MissingVars are the sorted caveat variables that leave a CONDITIONAL
	// entry unresolved. Empty for every other category.
	MissingVars []string `json:"missing_vars,omitempty"`

	// Expression is the normalized residual caveat expression for
	// CONDITIONAL entries.
	Expression string `json:"expression,omitempty"`

	// Message carries the CEL error text for CEL_ERROR entries.
	Message string `json:"message,omitempty"`
}

// SideRecord is one plan's full observable output for a triple.
type SideRecord struct {
	// Plan is the deterministic, data-only outline description of the plan
	// this side executed.
	Plan string `json:"plan"`

	// Failure is set when the side failed before producing an entry set.
	Failure *FailureRecord `json:"failure,omitempty"`

	// Entries are the normalized entries sorted by key. Entries are omitted
	// when Failure is set.
	Entries []EntryRecord `json:"entries,omitempty"`
}

// DeltaDebugRecord documents a bounded, deletion-only reduction run.
type DeltaDebugRecord struct {
	// Reruns is the number of paired executor reruns actually performed.
	Reruns int `json:"reruns"`

	// RerunLimitReached reports that reduction stopped because the rerun
	// budget was exhausted.
	RerunLimitReached bool `json:"rerun_limit_reached"`

	// RelationshipsRemoved is how many relationships the reduction deleted.
	RelationshipsRemoved int `json:"relationships_removed"`

	// ContextFieldsRemoved is how many top-level caveat context fields the
	// reduction deleted.
	ContextFieldsRemoved int `json:"context_fields_removed"`

	// NotGlobalMinimum states explicitly that ddmin-style reduction only
	// guarantees a locally irreducible input under deletions, not a global
	// minimum over all possible inputs.
	NotGlobalMinimum bool `json:"not_global_minimum"`
}

// Counterexample is the replayable artifact for the first divergent triple.
// Every field is plain JSON-serializable data; the artifact contains no
// functions and cannot execute code.
type Counterexample struct {
	// ArtifactVersion is the serialization version, always ArtifactVersion.
	ArtifactVersion string `json:"artifact_version"`

	// SchemaText is the fixed schema: the original DSL when supplied,
	// otherwise a deterministic rendering of the compiled definitions.
	SchemaText string `json:"schema_text"`

	// ModelName labels the finite model that exposed the divergence.
	ModelName string `json:"model_name"`

	// Relationships is the retained, normalized relationship list (sorted,
	// duplicate-free). After delta debugging this is the reduced input.
	Relationships []string `json:"relationships"`

	// CaveatContext is the context values the triple ran under, sanitized to
	// JSON-scalar values.
	CaveatContext map[string]any `json:"caveat_context"`

	// Request is the fixed request that exposed the divergence.
	Request RequestRecord `json:"request"`

	// Original is the reference plan's side output.
	Original SideRecord `json:"original"`

	// Candidate is the candidate plan's side output.
	Candidate SideRecord `json:"candidate"`

	// Difference is a human-readable summary of the first mismatching cell.
	Difference string `json:"difference"`

	// DeltaDebug is present only when reduction was enabled.
	DeltaDebug *DeltaDebugRecord `json:"delta_debug,omitempty"`

	// ScopeNote records the finite-model scope of the verdict so artifacts
	// cannot be mistaken for datastore-wide equivalence proofs.
	ScopeNote string `json:"scope_note"`
}

// RequestRecord is the JSON-safe rendering of a Request.
type RequestRecord struct {
	Operation       string `json:"operation"`
	ResourceType    string `json:"resource_type"`
	ResourceID      string `json:"resource_id"`
	SubjectType     string `json:"subject_type"`
	SubjectID       string `json:"subject_id"`
	SubjectRelation string `json:"subject_relation"`
	SubjectFilter   string `json:"subject_filter,omitempty"`
}

// PlanEquivalenceValidator validates plan equivalence over explicit finite
// models. A zero-value PlanEquivalenceValidator is not usable; construct one
// with New. The validator holds no mutable per-run state and is safe for
// concurrent use by independent validation runs.
type PlanEquivalenceValidator struct {
	cfg config
}

// New builds a validator with the given options. Default limits apply when no
// option overrides them; options can only tighten limits.
func New(opts ...Option) (*PlanEquivalenceValidator, error) {
	cfg := config{
		limits:         defaultLimits(),
		recursionDepth: defaultExecutionDepth,
	}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	return &PlanEquivalenceValidator{cfg: cfg}, nil
}

// operationName renders an operation for artifact records.
func operationName(op query.Operation) string {
	switch op {
	case query.OperationCheck:
		return "CHECK"
	case query.OperationIterResources:
		return "ITER_RESOURCES"
	case query.OperationIterSubjects:
		return "ITER_SUBJECTS"
	default:
		return "UNKNOWN"
	}
}

// canceled reports whether the context has already been canceled, in which
// case validation must stop rather than emit a counterexample.
func canceled(ctx context.Context) bool {
	return ctx.Err() != nil
}
