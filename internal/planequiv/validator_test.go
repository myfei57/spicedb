package planequiv_test

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/spicedb/internal/planequiv"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/query"
	"github.com/authzed/spicedb/pkg/query/queryopt"
	"github.com/authzed/spicedb/pkg/query/queryopt/optimization"
	"github.com/authzed/spicedb/pkg/query/queryopt/setsimplification"
	"github.com/authzed/spicedb/pkg/schema/v2"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
	"github.com/authzed/spicedb/pkg/tuple"
)

func parseSchema(t *testing.T, text string) (*schema.Schema, []*core.CaveatDefinition) {
	t.Helper()
	compiled, err := compiler.Compile(
		compiler.InputSchema{Source: input.Source("planequiv-test"), SchemaString: text},
		compiler.AllowUnprefixedObjectType(),
	)
	require.NoError(t, err)
	s, err := schema.BuildSchemaFromDefinitions(compiled.ObjectDefinitions, compiled.CaveatDefinitions)
	require.NoError(t, err)
	return s, compiled.CaveatDefinitions
}

func buildPlan(t *testing.T, s *schema.Schema, definition, permission string) query.CanonicalOutline {
	t.Helper()
	plan, err := query.BuildOutlineFromSchema(s, definition, permission)
	require.NoError(t, err)
	return plan
}

// recanonicalize applies a caller-chosen outline mutation and rebuilds node IDs
// exactly the way an external planner would. The validator itself never does
// this: the mutated plan is supplied as the candidate.
func recanonicalize(t *testing.T, plan query.CanonicalOutline, mutate func(query.Outline) query.Outline) query.CanonicalOutline {
	t.Helper()
	mutated, err := query.WalkOutlineBottomUp(plan.Root, func(node query.Outline) (query.Outline, error) {
		return mutate(node), nil
	})
	require.NoError(t, err)
	keys := make(map[query.OutlineNodeID]query.CanonicalKey, len(plan.CanonicalKeys))
	maps.Copy(keys, plan.CanonicalKeys)
	filled := query.FillMissingNodeIDs(mutated, keys)
	return query.CanonicalOutline{Root: filled, CanonicalKeys: keys}
}

func allRequests(resourceType, subjectType string, ids []string) []planequiv.Request {
	filter := query.NewType(subjectType, tuple.Ellipsis)
	requests := make([]planequiv.Request, 0, 3*len(ids))
	for _, id := range ids {
		resource := query.NewObject(resourceType, id)
		subject := query.NewObject(subjectType, id).WithEllipses()
		requests = append(requests,
			planequiv.Request{Operation: query.OperationCheck, Resource: resource, Subject: subject, SubjectFilter: filter},
			planequiv.Request{Operation: query.OperationIterSubjects, Resource: resource, Subject: subject, SubjectFilter: filter},
			planequiv.Request{Operation: query.OperationIterResources, Resource: resource, Subject: subject, SubjectFilter: filter},
		)
	}
	return requests
}

func mustRelationships(t *testing.T, texts ...string) []tuple.Relationship {
	t.Helper()
	relationships := make([]tuple.Relationship, 0, len(texts))
	for _, text := range texts {
		relationships = append(relationships, tuple.MustParse(text))
	}
	return relationships
}

// unconditionalFixedPlan builds a candidate that materializes the o_a grant
// without any caveat constraint: it models a planner that dropped the caveat
// and wrongly concluded access is unconditional.
func unconditionalFixedPlan(t *testing.T) query.CanonicalOutline {
	t.Helper()
	fixed := query.Outline{
		Type: query.FixedIteratorType,
		Args: &query.IteratorArgs{FixedPaths: []query.Path{{
			Resource: query.NewObject("document", "o_a"),
			Relation: "view",
			Subject:  query.NewObject("user", "o_a").WithEllipses(),
			Metadata: map[string]any{},
		}}},
	}
	plan, err := query.CanonicalizeOutline(fixed)
	require.NoError(t, err)
	return plan
}

func findEntry(records []planequiv.EntryRecord, classification planequiv.Classification) *planequiv.EntryRecord {
	for i := range records {
		if records[i].Classification == classification {
			return &records[i]
		}
	}
	return nil
}

// TestIdenticalPlansAreEquivalent covers union, intersection, exclusion and
// wildcard shapes end to end across all three operations.
func TestIdenticalPlansAreEquivalent(t *testing.T) {
	s, caveatDefs := parseSchema(t, `definition user {}
definition document {
 relation viewer: user
 relation editor: user
 relation banned: user
 permission view = viewer + editor - banned
}`)
	plan := buildPlan(t, s, "document", "view")
	relationships := mustRelationships(t,
		"document:o_a#viewer@user:o_a",
		"document:o_a#editor@user:o_b",
		"document:o_a#banned@user:o_c",
		"document:o_b#viewer@user:o_c",
	)

	validator, err := planequiv.New()
	require.NoError(t, err)
	report, err := validator.Validate(t.Context(), planequiv.Input{
		Schema:            s,
		CaveatDefinitions: caveatDefs,
		SchemaText:        "definition user {}",
		Original:          plan,
		Candidate:         plan,
		Models: []planequiv.FiniteModel{{
			Name:          "mixed",
			Relationships: relationships,
		}},
		Requests: allRequests("document", "user", []string{"o_a", "o_b", "absent"}),
	})
	require.NoError(t, err)
	require.True(t, report.Equivalent)
	require.Nil(t, report.Counterexample)
	require.Greater(t, report.ChecksEvaluated, 0)
}

// TestIntersectionRewrittenAsUnionFails proves the validator catches the
// classic set-op swap on every operation, not just Check.
func TestIntersectionRewrittenAsUnionFails(t *testing.T) {
	s, _ := parseSchema(t, `definition user {}
definition document {
 relation viewer: user
 relation editor: user
 permission view = viewer & editor
}`)
	plan := buildPlan(t, s, "document", "view")
	candidate := recanonicalize(t, plan, func(node query.Outline) query.Outline {
		if node.Type == query.IntersectionIteratorType {
			node.Type = query.UnionIteratorType
			node.ID = 0
		}
		return node
	})
	require.NotEqual(t, plan.Root.Serialize(), candidate.Root.Serialize())

	validator, err := planequiv.New()
	require.NoError(t, err)
	report, err := validator.Validate(t.Context(), planequiv.Input{
		Schema:    s,
		Original:  plan,
		Candidate: candidate,
		Models: []planequiv.FiniteModel{{
			Relationships: mustRelationships(t,
				"document:o_a#viewer@user:o_a",
				"document:o_b#viewer@user:o_b",
				"document:o_b#editor@user:o_b",
			),
		}},
		Requests: allRequests("document", "user", []string{"o_a", "o_b"}),
	})
	require.NoError(t, err)
	require.False(t, report.Equivalent)
	counterexample := report.Counterexample
	require.NotNil(t, counterexample)
	require.Equal(t, planequiv.ArtifactVersion, counterexample.ArtifactVersion)
	require.NotEmpty(t, counterexample.Relationships)
	require.NotNil(t, findEntry(counterexample.Original.Entries, planequiv.ClassNo))
	require.NotNil(t, findEntry(counterexample.Candidate.Entries, planequiv.ClassHas))
	require.Contains(t, counterexample.Difference, "differs")
	require.NotEmpty(t, counterexample.ScopeNote)
}

// TestDroppingExclusionFails: removing the subtree of an exclusion must grant
// a banned wildcard subject access that the reference plan denies.
func TestDroppingExclusionFails(t *testing.T) {
	s, _ := parseSchema(t, `definition user {}
definition document {
 relation viewer: user:*
 relation banned: user
 permission view = viewer - banned
}`)
	plan := buildPlan(t, s, "document", "view")
	candidate := recanonicalize(t, plan, func(node query.Outline) query.Outline {
		if node.Type == query.ExclusionIteratorType && len(node.SubOutlines) == 2 {
			return node.SubOutlines[0]
		}
		return node
	})

	validator, err := planequiv.New()
	require.NoError(t, err)
	report, err := validator.Validate(t.Context(), planequiv.Input{
		Schema:    s,
		Original:  plan,
		Candidate: candidate,
		Models: []planequiv.FiniteModel{{
			Relationships: mustRelationships(t,
				"document:o_a#viewer@user:*",
				"document:o_a#banned@user:o_a",
			),
		}},
		Requests: allRequests("document", "user", []string{"o_a"}),
	})
	require.NoError(t, err)
	require.False(t, report.Equivalent)
	counterexample := report.Counterexample
	require.NotNil(t, counterexample)
	require.NotNil(t, findEntry(counterexample.Original.Entries, planequiv.ClassNo))
	require.NotNil(t, findEntry(counterexample.Candidate.Entries, planequiv.ClassHas))
}

// TestDroppingCaveatFails: a stripped caveat turns a CONDITIONAL grant into an
// unconditional HAS under a partial context. Unknown never passes as wildcard.
func TestDroppingCaveatFails(t *testing.T) {
	s, caveatDefs := parseSchema(t, `caveat enabled(ok bool) { ok }
definition user {}
definition document {
 relation viewer: user with enabled
 permission view = viewer
}`)
	plan := buildPlan(t, s, "document", "view")
	candidate := unconditionalFixedPlan(t)
	relationships := mustRelationships(t, "document:o_a#viewer@user:o_a[enabled]")
	requests := allRequests("document", "user", []string{"o_a"})

	t.Run("partial context diverges", func(t *testing.T) {
		validator, err := planequiv.New()
		require.NoError(t, err)
		report, err := validator.Validate(t.Context(), planequiv.Input{
			Schema:            s,
			CaveatDefinitions: caveatDefs,
			Original:          plan,
			Candidate:         candidate,
			Models: []planequiv.FiniteModel{{
				Relationships: relationships,
				CaveatContexts: []map[string]any{
					nil,
					{"ok": true},
					{"ok": false},
				},
			}},
			Requests: requests,
		})
		require.NoError(t, err)
		require.False(t, report.Equivalent)
		counterexample := report.Counterexample
		require.NotNil(t, counterexample)
		conditional := findEntry(counterexample.Original.Entries, planequiv.ClassConditional)
		require.NotNil(t, conditional)
		require.Equal(t, []string{"ok"}, conditional.MissingVars)
		require.NotNil(t, findEntry(counterexample.Candidate.Entries, planequiv.ClassHas))
	})

	t.Run("satisfied context matches", func(t *testing.T) {
		validator, err := planequiv.New()
		require.NoError(t, err)
		report, err := validator.Validate(t.Context(), planequiv.Input{
			Schema:            s,
			CaveatDefinitions: caveatDefs,
			Original:          plan,
			Candidate:         candidate,
			Models: []planequiv.FiniteModel{{
				Relationships:  relationships,
				CaveatContexts: []map[string]any{{"ok": true}},
			}},
			Requests: requests,
		})
		require.NoError(t, err)
		require.True(t, report.Equivalent)
	})

	t.Run("violated context diverges", func(t *testing.T) {
		// With ok=false the reference plan denies (caveat false suppresses the
		// path) while the caveat-stripped candidate still grants: the two
		// membership classes must not be treated as equal.
		validator, err := planequiv.New()
		require.NoError(t, err)
		report, err := validator.Validate(t.Context(), planequiv.Input{
			Schema:            s,
			CaveatDefinitions: caveatDefs,
			Original:          plan,
			Candidate:         candidate,
			Models: []planequiv.FiniteModel{{
				Relationships:  relationships,
				CaveatContexts: []map[string]any{{"ok": false}},
			}},
			Requests: requests,
		})
		require.NoError(t, err)
		require.False(t, report.Equivalent)
	})
}

// TestCELErrorIsItsOwnClass pins separation of CEL failures from membership:
// identical plans both fail with CEL_ERROR and match on failure class, while a
// caveat-stripped candidate diverges because its side answers HAS.
func TestCELErrorIsItsOwnClass(t *testing.T) {
	s, caveatDefs := parseSchema(t, `caveat enabled(ok bool) { ok }
definition user {}
definition document {
 relation viewer: user with enabled
 permission view = viewer
}`)
	plan := buildPlan(t, s, "document", "view")
	requests := allRequests("document", "user", []string{"o_a"})[0:1]
	relationships := mustRelationships(t, "document:o_a#viewer@user:o_a[enabled]")
	badContext := []map[string]any{{"ok": "not-a-bool"}}

	t.Run("matching CEL failures are equivalent", func(t *testing.T) {
		validator, err := planequiv.New()
		require.NoError(t, err)
		report, err := validator.Validate(t.Context(), planequiv.Input{
			Schema:            s,
			CaveatDefinitions: caveatDefs,
			Original:          plan,
			Candidate:         plan,
			Models: []planequiv.FiniteModel{{
				Relationships:  relationships,
				CaveatContexts: badContext,
			}},
			Requests: requests,
		})
		require.NoError(t, err)
		require.True(t, report.Equivalent)
	})

	t.Run("CEL failure versus HAS diverges", func(t *testing.T) {
		candidate := unconditionalFixedPlan(t)
		validator, err := planequiv.New()
		require.NoError(t, err)
		report, err := validator.Validate(t.Context(), planequiv.Input{
			Schema:            s,
			CaveatDefinitions: caveatDefs,
			Original:          plan,
			Candidate:         candidate,
			Models: []planequiv.FiniteModel{{
				Relationships:  relationships,
				CaveatContexts: badContext,
			}},
			Requests: requests,
		})
		require.NoError(t, err)
		require.False(t, report.Equivalent)
		require.Contains(t, report.Counterexample.Difference, "failure class differs")
		require.Equal(t, planequiv.ClassCELError, report.Counterexample.Original.Failure.Kind)
		require.Nil(t, report.Counterexample.Candidate.Failure)
	})
}

// TestRealOptimizerProducesEquivalentCandidate shows the intended workflow:
// the caller runs an optimizer itself and hands the optimized plan to the
// validator, which never invokes an optimizer.
func TestRealOptimizerProducesEquivalentCandidate(t *testing.T) {
	s, _ := parseSchema(t, `definition user {}
definition document {
 relation viewer: user
 relation editor: user
 permission view = viewer + (viewer & editor)
}`)
	plan := buildPlan(t, s, "document", "view")
	candidate, err := queryopt.ApplyOptimizations(plan,
		[]queryopt.Optimizer{setsimplification.New()},
		optimization.RequestParams{Operation: query.OperationCheck, SubjectType: "user", SubjectRelation: tuple.Ellipsis},
	)
	require.NoError(t, err)
	require.NotEqual(t, plan.Root.Serialize(), candidate.Root.Serialize())

	validator, err := planequiv.New()
	require.NoError(t, err)
	report, err := validator.Validate(t.Context(), planequiv.Input{
		Schema:    s,
		Original:  plan,
		Candidate: candidate,
		Models: []planequiv.FiniteModel{{
			Relationships: mustRelationships(t,
				"document:o_a#viewer@user:o_a",
				"document:o_a#editor@user:o_a",
				"document:o_b#editor@user:o_b",
			),
		}},
		Requests: allRequests("document", "user", []string{"o_a", "o_b", "absent"}),
	})
	require.NoError(t, err)
	require.True(t, report.Equivalent)
}

// TestDeltaDebuggingRetainsOnlyRelevantInput verifies deletion-only reduction
// reruns the real executor, removes irrelevant relationships, stays bounded
// and never claims a global minimum.
func TestDeltaDebuggingRetainsOnlyRelevantInput(t *testing.T) {
	s, _ := parseSchema(t, `definition user {}
definition document {
 relation viewer: user
 relation editor: user
 permission view = viewer & editor
}`)
	plan := buildPlan(t, s, "document", "view")
	candidate := recanonicalize(t, plan, func(node query.Outline) query.Outline {
		if node.Type == query.IntersectionIteratorType {
			node.Type = query.UnionIteratorType
			node.ID = 0
		}
		return node
	})
	relationships := mustRelationships(t,
		"document:o_a#viewer@user:o_a",
		"document:noise1#viewer@user:noise1",
		"document:noise2#editor@user:noise2",
	)

	validator, err := planequiv.New(planequiv.WithDeltaDebugging(32))
	require.NoError(t, err)
	report, err := validator.Validate(t.Context(), planequiv.Input{
		Schema:    s,
		Original:  plan,
		Candidate: candidate,
		Models:    []planequiv.FiniteModel{{Relationships: relationships}},
		Requests:  allRequests("document", "user", []string{"o_a"}),
	})
	require.NoError(t, err)
	require.False(t, report.Equivalent)
	counterexample := report.Counterexample
	require.NotNil(t, counterexample.DeltaDebug)
	require.True(t, counterexample.DeltaDebug.NotGlobalMinimum)
	require.LessOrEqual(t, counterexample.DeltaDebug.Reruns, 32)
	require.Greater(t, counterexample.DeltaDebug.RelationshipsRemoved, 0)
	require.Len(t, counterexample.Relationships, len(relationships)-counterexample.DeltaDebug.RelationshipsRemoved)

	// The retained reduced input must still reproduce the divergence through a
	// fresh validator run against the real executor.
	retained := mustRelationships(t, counterexample.Relationships...)
	replayValidator, err := planequiv.New()
	require.NoError(t, err)
	replay, err := replayValidator.Validate(t.Context(), planequiv.Input{
		Schema:    s,
		Original:  plan,
		Candidate: candidate,
		Models:    []planequiv.FiniteModel{{Relationships: retained, CaveatContexts: []map[string]any{counterexample.CaveatContext}}},
		Requests:  allRequests("document", "user", []string{"o_a"}),
	})
	require.NoError(t, err)
	require.False(t, replay.Equivalent)
}

// TestDeltaDebuggingRerunCap ensures reduction never exceeds its rerun budget.
func TestDeltaDebuggingRerunCap(t *testing.T) {
	s, _ := parseSchema(t, `definition user {}
definition document {
 relation viewer: user
 relation editor: user
 permission view = viewer & editor
}`)
	plan := buildPlan(t, s, "document", "view")
	candidate := recanonicalize(t, plan, func(node query.Outline) query.Outline {
		if node.Type == query.IntersectionIteratorType {
			node.Type = query.UnionIteratorType
			node.ID = 0
		}
		return node
	})
	relationships := mustRelationships(t,
		"document:o_a#viewer@user:o_a",
		"document:n1#viewer@user:n1",
		"document:n2#viewer@user:n2",
		"document:n3#editor@user:n3",
	)

	validator, err := planequiv.New(planequiv.WithDeltaDebugging(1))
	require.NoError(t, err)
	report, err := validator.Validate(t.Context(), planequiv.Input{
		Schema:    s,
		Original:  plan,
		Candidate: candidate,
		Models:    []planequiv.FiniteModel{{Relationships: relationships}},
		Requests:  allRequests("document", "user", []string{"o_a"}),
	})
	require.NoError(t, err)
	require.False(t, report.Equivalent)
	require.NotNil(t, report.Counterexample.DeltaDebug)
	require.LessOrEqual(t, report.Counterexample.DeltaDebug.Reruns, 1)
}

// TestDeltaDebuggingRemovesIrrelevantContextFields covers the second reduction
// axis: top-level caveat context fields are deleted one at a time and retained
// only while the divergence survives an executor rerun.
func TestDeltaDebuggingRemovesIrrelevantContextFields(t *testing.T) {
	s, caveatDefs := parseSchema(t, `caveat enabled(ok bool) { ok }
definition user {}
definition document {
 relation viewer: user with enabled
 permission view = viewer
}`)
	plan := buildPlan(t, s, "document", "view")
	candidate := unconditionalFixedPlan(t)

	validator, err := planequiv.New(planequiv.WithDeltaDebugging(16))
	require.NoError(t, err)
	report, err := validator.Validate(t.Context(), planequiv.Input{
		Schema:            s,
		CaveatDefinitions: caveatDefs,
		Original:          plan,
		Candidate:         candidate,
		Models: []planequiv.FiniteModel{{
			Relationships:  mustRelationships(t, "document:o_a#viewer@user:o_a[enabled]"),
			CaveatContexts: []map[string]any{{"ok": false, "irrelevant": "drop-me"}},
		}},
		Requests: allRequests("document", "user", []string{"o_a"}),
	})
	require.NoError(t, err)
	require.False(t, report.Equivalent)
	record := report.Counterexample.DeltaDebug
	require.NotNil(t, record)
	require.GreaterOrEqual(t, record.ContextFieldsRemoved, 1)
	require.NotContains(t, report.Counterexample.CaveatContext, "irrelevant")
	require.True(t, record.NotGlobalMinimum)
	require.LessOrEqual(t, record.Reruns, 16)
}

// TestBudgetExhaustionIsNotAnEmptyResult ensures recursion-budget failures are
// a distinct, matchable class rather than being treated as NO.
func TestBudgetExhaustionIsNotAnEmptyResult(t *testing.T) {
	s, _ := parseSchema(t, `definition user {}
definition folder {
 relation parent: folder
 relation viewer: user
 permission view = viewer + parent->view
}`)
	plan := buildPlan(t, s, "folder", "view")
	relationships := mustRelationships(t,
		"folder:f1#parent@folder:f2",
		"folder:f2#parent@folder:f1",
	)

	validator, err := planequiv.New(planequiv.WithRecursionDepth(1))
	require.NoError(t, err)
	report, err := validator.Validate(t.Context(), planequiv.Input{
		Schema:    s,
		Original:  plan,
		Candidate: plan,
		Models:    []planequiv.FiniteModel{{Relationships: relationships}},
		Requests:  allRequests("folder", "user", []string{"f1"}),
	})
	require.NoError(t, err)
	require.True(t, report.Equivalent, "identical plans must match when both exhaust the recursion budget")
}

func TestLimitsAreEnforced(t *testing.T) {
	_, err := planequiv.New(planequiv.WithMaxModels(0))
	require.Error(t, err)
	_, err = planequiv.New(planequiv.WithMaxPlanDepth(1 << 20))
	require.Error(t, err)
	_, err = planequiv.New(planequiv.WithDeltaDebugging(-1))
	require.Error(t, err)

	s, _ := parseSchema(t, `definition user {}
definition document {
 relation viewer: user
 permission view = viewer
}`)
	plan := buildPlan(t, s, "document", "view")
	base := planequiv.Input{
		Schema:    s,
		Original:  plan,
		Candidate: plan,
		Models:    []planequiv.FiniteModel{{Relationships: mustRelationships(t, "document:o_a#viewer@user:o_a")}},
		Requests:  allRequests("document", "user", []string{"o_a"}),
	}

	validator, err := planequiv.New(planequiv.WithMaxModels(1))
	require.NoError(t, err)
	tooManyModels := base
	tooManyModels.Models = []planequiv.FiniteModel{{}, {}}
	_, err = validator.Validate(t.Context(), tooManyModels)
	require.ErrorContains(t, err, "model count")

	validator, err = planequiv.New(planequiv.WithMaxRelationships(1))
	require.NoError(t, err)
	tooManyRels := base
	tooManyRels.Models = []planequiv.FiniteModel{{Relationships: mustRelationships(t,
		"document:o_a#viewer@user:o_a",
		"document:o_b#viewer@user:o_b",
	)}}
	_, err = validator.Validate(t.Context(), tooManyRels)
	require.ErrorContains(t, err, "relationships")

	validator, err = planequiv.New(planequiv.WithMaxPlanDepth(1))
	require.NoError(t, err)
	_, err = validator.Validate(t.Context(), base)
	require.ErrorContains(t, err, "depth")

	empty := base
	empty.Models = nil
	validator, err = planequiv.New()
	require.NoError(t, err)
	_, err = validator.Validate(t.Context(), empty)
	require.ErrorContains(t, err, "finite model")
}

func TestCancelationStopsValidation(t *testing.T) {
	s, _ := parseSchema(t, `definition user {}
definition document {
 relation viewer: user
 permission view = viewer
}`)
	plan := buildPlan(t, s, "document", "view")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	validator, err := planequiv.New()
	require.NoError(t, err)
	_, err = validator.Validate(ctx, planequiv.Input{
		Schema:    s,
		Original:  plan,
		Candidate: plan,
		Models: []planequiv.FiniteModel{{
			Relationships: mustRelationships(t, "document:o_a#viewer@user:o_a"),
		}},
		Requests: allRequests("document", "user", []string{"o_a"}),
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestArtifactIsVersionedJSONAndSizeCapped(t *testing.T) {
	s, _ := parseSchema(t, `definition user {}
definition document {
 relation viewer: user
 relation editor: user
 permission view = viewer & editor
}`)
	plan := buildPlan(t, s, "document", "view")
	candidate := recanonicalize(t, plan, func(node query.Outline) query.Outline {
		if node.Type == query.IntersectionIteratorType {
			node.Type = query.UnionIteratorType
			node.ID = 0
		}
		return node
	})

	validator, err := planequiv.New()
	require.NoError(t, err)
	report, err := validator.Validate(t.Context(), planequiv.Input{
		Schema:    s,
		Original:  plan,
		Candidate: candidate,
		Models: []planequiv.FiniteModel{{
			Relationships: mustRelationships(t, "document:o_a#viewer@user:o_a"),
		}},
		Requests: allRequests("document", "user", []string{"o_a"}),
	})
	require.NoError(t, err)
	require.False(t, report.Equivalent)

	raw, err := validator.MarshalArtifact(report.Counterexample)
	require.NoError(t, err)
	require.NotEmpty(t, raw)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, planequiv.ArtifactVersion, decoded["artifact_version"])
	require.Contains(t, decoded, "original")
	require.Contains(t, decoded, "candidate")
	require.Contains(t, decoded, "request")

	tiny, err := planequiv.New(planequiv.WithMaxArtifactBytes(1))
	require.NoError(t, err)
	_, err = tiny.MarshalArtifact(report.Counterexample)
	require.ErrorContains(t, err, "exceeding")
}

func TestValidatorDoesNotMutateCallerPlans(t *testing.T) {
	s, _ := parseSchema(t, `definition user {}
definition document {
 relation viewer: user
 relation editor: user
 permission view = viewer & editor
}`)
	plan := buildPlan(t, s, "document", "view")
	candidate := recanonicalize(t, plan, func(node query.Outline) query.Outline {
		if node.Type == query.IntersectionIteratorType {
			node.Type = query.UnionIteratorType
			node.ID = 0
		}
		return node
	})
	originalKey := plan.Root.Serialize()
	candidateKey := candidate.Root.Serialize()

	validator, err := planequiv.New(planequiv.WithDeltaDebugging(8))
	require.NoError(t, err)
	_, err = validator.Validate(t.Context(), planequiv.Input{
		Schema:    s,
		Original:  plan,
		Candidate: candidate,
		Models: []planequiv.FiniteModel{{
			Relationships: mustRelationships(t, "document:o_a#viewer@user:o_a"),
		}},
		Requests: allRequests("document", "user", []string{"o_a"}),
	})
	require.NoError(t, err)
	require.Equal(t, originalKey, plan.Root.Serialize())
	require.Equal(t, candidateKey, candidate.Root.Serialize())
}
