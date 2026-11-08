package planequiv

import (
	"context"
	"fmt"

	"github.com/authzed/spicedb/internal/caveats"
	"github.com/authzed/spicedb/pkg/datalayer"
	"github.com/authzed/spicedb/pkg/query"
)

// mismatch describes why two sides diverged for one triple. Exactly one of
// entryDiff or the failure fields is populated.
type mismatch struct {
	reason       string
	entryDiff    *entryDiff
	leftFailure  *FailureRecord
	rightFailure *FailureRecord
}

// tripleOutcome is the full result of executing and normalizing one triple on
// both sides.
type tripleOutcome struct {
	left         sideResult
	right        sideResult
	leftEntries  map[string]semanticEntry
	rightEntries map[string]semanticEntry
	domain       []string
	mismatch     *mismatch
	canceled     bool
}

// Validate executes both plans over every (model, request, context) triple and
// returns the first replayable counterexample. A divergence is reported on the
// Report, not as a Go error: errors are reserved for invalid input, setup
// failures and cancellation. The validator never applies an optimizer and
// never mutates the caller's plans.
func (v *PlanEquivalenceValidator) Validate(ctx context.Context, in Input) (*Report, error) {
	if err := v.validateInput(in); err != nil {
		return nil, err
	}

	report := &Report{}
	for _, model := range in.Models {
		if canceled(ctx) {
			return nil, ctx.Err()
		}

		store, err := buildModelStore(ctx, in.Schema, in.CaveatDefinitions, model.Relationships)
		if err != nil {
			return nil, fmt.Errorf("planequiv: model %q setup failed: %w", model.Name, err)
		}

		schemaReader, err := store.reader.ReadSchema(ctx)
		if err != nil {
			store.close()
			return nil, err
		}

		contexts := model.CaveatContexts
		if len(contexts) == 0 {
			contexts = []map[string]any{nil}
		}

		for _, req := range in.Requests {
			for _, caveatContext := range contexts {
				if canceled(ctx) {
					store.close()
					return nil, ctx.Err()
				}

				report.ChecksEvaluated++
				outcome := v.runTriple(ctx, store.reader, schemaReader, in, req, caveatContext)
				if outcome.canceled {
					store.close()
					return nil, ctx.Err()
				}
				if outcome.mismatch != nil {
					counterexample, err := v.buildCounterexample(in, model, req, caveatContext, outcome)
					if err != nil {
						store.close()
						return nil, err
					}

					if v.cfg.deltaDebug {
						v.applyReduction(ctx, in, model, req, caveatContext, outcome, counterexample)
					}

					report.Equivalent = false
					report.Counterexample = counterexample
					store.close()
					return report, nil
				}
			}
		}
		store.close()
	}

	report.Equivalent = true
	return report, nil
}

// runTriple executes both plans independently and compares their normalized
// observable outputs for one triple.
func (v *PlanEquivalenceValidator) runTriple(ctx context.Context, reader datalayer.RevisionedReader, schemaReader caveats.CaveatDefinitionLookup, in Input, req Request, caveatContext map[string]any) tripleOutcome {
	outcome := tripleOutcome{}

	outcome.left = v.executeSide(ctx, reader, in.Original, req, caveatContext)
	if outcome.left.failure != nil && outcome.left.failure.Kind == ClassCanceled {
		outcome.canceled = true
		return outcome
	}
	outcome.right = v.executeSide(ctx, reader, in.Candidate, req, caveatContext)
	if outcome.right.failure != nil && outcome.right.failure.Kind == ClassCanceled {
		outcome.canceled = true
		return outcome
	}

	// Whole-side failures are a distinct observable class. Matching failure
	// classes count as equivalent; every other combination is a divergence.
	if outcome.left.failure != nil || outcome.right.failure != nil {
		if outcome.left.failure == nil || outcome.right.failure == nil ||
			outcome.left.failure.Kind != outcome.right.failure.Kind {
			outcome.mismatch = &mismatch{
				reason:       failureMismatchReason(outcome.left.failure, outcome.right.failure),
				leftFailure:  outcome.left.failure,
				rightFailure: outcome.right.failure,
			}
		}
		return outcome
	}

	outcome.domain = subjectDomain(req.Operation, req.Subject, outcome.left.paths, outcome.right.paths)

	var leftFailure, rightFailure *FailureRecord
	outcome.leftEntries, leftFailure = normalizeSide(ctx, schemaReader, req.Operation, req.Subject, outcome.domain, outcome.left.paths, caveatContext)
	if leftFailure != nil {
		if leftFailure.Kind == ClassCanceled {
			outcome.canceled = true
			return outcome
		}
		outcome.left.failure = leftFailure
	}
	outcome.rightEntries, rightFailure = normalizeSide(ctx, schemaReader, req.Operation, req.Subject, outcome.domain, outcome.right.paths, caveatContext)
	if rightFailure != nil {
		if rightFailure.Kind == ClassCanceled {
			outcome.canceled = true
			return outcome
		}
		outcome.right.failure = rightFailure
	}
	if leftFailure != nil || rightFailure != nil {
		outcome.mismatch = &mismatch{
			reason:       failureMismatchReason(leftFailure, rightFailure),
			leftFailure:  leftFailure,
			rightFailure: rightFailure,
		}
		return outcome
	}

	if diff := compareEntries(outcome.leftEntries, outcome.rightEntries); diff != nil {
		outcome.mismatch = &mismatch{reason: diff.reason, entryDiff: diff}
	}
	return outcome
}

// failureMismatchReason renders a deterministic summary when the two sides'
// failure classes differ.
func failureMismatchReason(left, right *FailureRecord) string {
	leftKind, rightKind := ClassNo, ClassNo
	if left != nil {
		leftKind = left.Kind
	}
	if right != nil {
		rightKind = right.Kind
	}
	return fmt.Sprintf("failure class differs: original=%s candidate=%s", leftKind, rightKind)
}

// applyReduction runs the bounded deletion-only reduction and rewrites the
// artifact's retained input and side outputs to the last confirmed-diverging
// rerun, so the replay artifact stays internally coherent.
func (v *PlanEquivalenceValidator) applyReduction(ctx context.Context, in Input, model FiniteModel, req Request, caveatContext map[string]any, outcome tripleOutcome, counterexample *Counterexample) {
	harness := &reductionHarness{
		v:             v,
		ctx:           ctx,
		input:         in,
		request:       req,
		caveatContext: caveatContext,
		remaining:     v.cfg.deltaReruns,
	}

	reducedRelationships := harness.reduceRelationships(model.Relationships)
	reducedContext, contextFieldsRemoved := harness.reduceContextFields(reducedRelationships)

	record := &DeltaDebugRecord{
		Reruns:               harness.reruns,
		RerunLimitReached:    harness.capped,
		RelationshipsRemoved: len(model.Relationships) - len(reducedRelationships),
		ContextFieldsRemoved: contextFieldsRemoved,
		NotGlobalMinimum:     true,
	}

	rendered, err := normalizeRelationships(reducedRelationships)
	if err == nil {
		counterexample.Relationships = rendered
	}
	sanitized, err := sanitizeContext(reducedContext)
	if err == nil {
		counterexample.CaveatContext = sanitized
	}
	if harness.lastDiverging != nil {
		rewriteSideRecords(counterexample, in, req, *harness.lastDiverging)
	}
	counterexample.DeltaDebug = record
}

// rewriteSideRecords replaces an artifact's side outputs with those of a
// confirmed post-reduction rerun.
func rewriteSideRecords(counterexample *Counterexample, in Input, req Request, outcome tripleOutcome) {
	keys := jointKeys(outcome.leftEntries, outcome.rightEntries)
	counterexample.Original = buildSideRecord(in.Original, outcome.left, outcome.leftEntries, keys)
	counterexample.Candidate = buildSideRecord(in.Candidate, outcome.right, outcome.rightEntries, keys)
	if outcome.mismatch != nil {
		counterexample.Difference = outcome.mismatch.reason
	}
}

// buildCounterexample assembles the data-only replay artifact for the first
// divergent triple.
func (v *PlanEquivalenceValidator) buildCounterexample(in Input, model FiniteModel, req Request, caveatContext map[string]any, outcome tripleOutcome) (*Counterexample, error) {
	schemaText, err := renderSchema(in.Schema, in.SchemaText)
	if err != nil {
		return nil, err
	}
	relationships, err := normalizeRelationships(model.Relationships)
	if err != nil {
		return nil, err
	}
	sanitizedContext, err := sanitizeContext(caveatContext)
	if err != nil {
		return nil, err
	}

	keys := jointKeys(outcome.leftEntries, outcome.rightEntries)
	counterexample := &Counterexample{
		ArtifactVersion: ArtifactVersion,
		SchemaText:      schemaText,
		ModelName:       model.Name,
		Relationships:   relationships,
		CaveatContext:   sanitizedContext,
		Request:         requestRecord(req),
		Original:        buildSideRecord(in.Original, outcome.left, outcome.leftEntries, keys),
		Candidate:       buildSideRecord(in.Candidate, outcome.right, outcome.rightEntries, keys),
		Difference:      outcome.mismatch.reason,
		ScopeNote:       scopeNote,
	}
	return counterexample, nil
}

// buildSideRecord renders one side, preferring its failure record when
// execution did not produce an entry set.
func buildSideRecord(plan query.CanonicalOutline, result sideResult, entries map[string]semanticEntry, keys []string) SideRecord {
	record := SideRecord{Plan: renderOutline(plan.Root)}
	if result.failure != nil {
		record.Failure = result.failure
		return record
	}
	record.Entries = toRecord(entries, keys)
	return record
}

// validateInput enforces every configured limit and rejects inputs that could
// produce oversized or non-serializable artifacts.
func (v *PlanEquivalenceValidator) validateInput(in Input) error {
	if in.Schema == nil {
		return fmt.Errorf("planequiv: Input.Schema is required")
	}
	if len(in.Models) == 0 {
		return fmt.Errorf("planequiv: at least one finite model is required")
	}
	if len(in.Models) > v.cfg.limits.maxModels {
		return fmt.Errorf("planequiv: model count %d exceeds limit %d", len(in.Models), v.cfg.limits.maxModels)
	}
	if len(in.Requests) == 0 {
		return fmt.Errorf("planequiv: at least one request is required")
	}
	if len(in.Requests) > v.cfg.limits.maxRequests {
		return fmt.Errorf("planequiv: request count %d exceeds limit %d", len(in.Requests), v.cfg.limits.maxRequests)
	}

	if err := checkOutline("original", in.Original.Root, v.cfg.limits); err != nil {
		return err
	}
	if err := checkOutline("candidate", in.Candidate.Root, v.cfg.limits); err != nil {
		return err
	}

	for modelIndex, model := range in.Models {
		if len(model.Relationships) > v.cfg.limits.maxRelationships {
			return fmt.Errorf("planequiv: model %d has %d relationships, exceeding limit %d", modelIndex, len(model.Relationships), v.cfg.limits.maxRelationships)
		}
		if len(model.CaveatContexts) > v.cfg.limits.maxContexts {
			return fmt.Errorf("planequiv: model %d has %d caveat contexts, exceeding limit %d", modelIndex, len(model.CaveatContexts), v.cfg.limits.maxContexts)
		}
		if len(model.Name) > v.cfg.limits.maxStringLength {
			return fmt.Errorf("planequiv: model %d name exceeds %d bytes", modelIndex, v.cfg.limits.maxStringLength)
		}
		for contextIndex, values := range model.CaveatContexts {
			if err := checkContext(modelIndex, contextIndex, values, v.cfg.limits); err != nil {
				return err
			}
		}
	}

	for reqIndex, req := range in.Requests {
		switch req.Operation {
		case query.OperationCheck, query.OperationIterResources, query.OperationIterSubjects:
		default:
			return fmt.Errorf("planequiv: request %d uses unsupported operation %v", reqIndex, req.Operation)
		}
	}
	return nil
}

// checkContext validates context field counts, key/value sizes and JSON
// serializability.
func checkContext(modelIndex, contextIndex int, values map[string]any, lim limits) error {
	if len(values) > lim.maxContextFields {
		return fmt.Errorf("planequiv: model %d context %d has %d fields, exceeding limit %d", modelIndex, contextIndex, len(values), lim.maxContextFields)
	}
	for key, value := range values {
		if len(key) > lim.maxStringLength {
			return fmt.Errorf("planequiv: model %d context %d key %q exceeds %d bytes", modelIndex, contextIndex, key, lim.maxStringLength)
		}
		if text, ok := value.(string); ok && len(text) > lim.maxStringLength {
			return fmt.Errorf("planequiv: model %d context %d value for %q exceeds %d bytes", modelIndex, contextIndex, key, lim.maxStringLength)
		}
	}
	if _, err := sanitizeContext(values); err != nil {
		return fmt.Errorf("planequiv: model %d context %d: %w", modelIndex, contextIndex, err)
	}
	return nil
}

// checkOutline walks a plan tree and enforces depth and node caps.
func checkOutline(label string, root query.Outline, lim limits) error {
	nodes := 0
	var walk func(node query.Outline, depth int) error
	walk = func(node query.Outline, depth int) error {
		if depth > lim.maxPlanDepth {
			return fmt.Errorf("planequiv: %s plan exceeds max depth %d", label, lim.maxPlanDepth)
		}
		nodes++
		if nodes > lim.maxPlanNodes {
			return fmt.Errorf("planequiv: %s plan exceeds max node count %d", label, lim.maxPlanNodes)
		}
		for _, child := range node.SubOutlines {
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root, 1)
}
