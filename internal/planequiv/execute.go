package planequiv

import (
	"context"
	"errors"

	"github.com/authzed/spicedb/internal/caveats"
	pkgcaveats "github.com/authzed/spicedb/pkg/caveats"
	"github.com/authzed/spicedb/pkg/caveats/types"
	"github.com/authzed/spicedb/pkg/datalayer"
	"github.com/authzed/spicedb/pkg/query"
)

// sideResult is everything one plan produced for one triple. Exactly one of
// failure (whole-side failure) or a path set is meaningful; a successful run
// may still return zero paths.
type sideResult struct {
	paths   []*query.Path
	failure *FailureRecord
}

// classifyFailure maps an executor error onto one of the separated failure
// classes. CEL failures, recursion-budget exhaustion and cancellation never
// collapse into a generic error, so a difference in failure mode is itself a
// semantic divergence.
func classifyFailure(err error) *FailureRecord {
	if err == nil {
		return nil
	}

	var depthErr query.MaxRecursionDepthError
	if errors.As(err, &depthErr) {
		return &FailureRecord{Kind: ClassBudgetExhausted, Message: err.Error()}
	}

	var internalEval caveats.EvaluationError
	if errors.As(err, &internalEval) {
		return &FailureRecord{Kind: ClassCELError, Message: err.Error()}
	}
	var internalParam caveats.ParameterTypeError
	if errors.As(err, &internalParam) {
		return &FailureRecord{Kind: ClassCELError, Message: err.Error()}
	}
	var evalErr pkgcaveats.EvaluationError
	if errors.As(err, &evalErr) {
		return &FailureRecord{Kind: ClassCELError, Message: err.Error()}
	}
	var paramErr pkgcaveats.ParameterConversionError
	if errors.As(err, &paramErr) {
		return &FailureRecord{Kind: ClassCELError, Message: err.Error()}
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &FailureRecord{Kind: ClassCanceled, Message: err.Error()}
	}

	return &FailureRecord{Kind: ClassExecutionError, Message: err.Error()}
}

// drain fully realizes a PathSeq while honoring cancellation. Returning early
// (on error or cancel) makes the range-over-func iterator observe a false
// yield and run its deferred cleanup, so no iterator is left open; the
// caller's deferred side context cancel releases any blocked datastore reads.
func drain(ctx context.Context, seq query.PathSeq) ([]*query.Path, *FailureRecord) {
	var paths []*query.Path
	for path, err := range seq {
		if err != nil {
			return paths, classifyFailure(err)
		}
		if err := ctx.Err(); err != nil {
			return paths, classifyFailure(err)
		}
		if path == nil {
			continue
		}
		paths = append(paths, path)
	}
	if err := ctx.Err(); err != nil {
		return paths, classifyFailure(err)
	}
	return paths, nil
}

// executeSide compiles and runs one caller-supplied plan on a fresh local
// query context. Each side gets:
//   - its own compiled iterator tree (the plans are never shared or rewritten),
//   - its own query.Context, caveat runner and cancelable Go context,
//   - the same read-only snapshot reader (the finite model under test).
//
// The deferred cancel guarantees iterator teardown on every return path,
// including early cancellation.
func (v *PlanEquivalenceValidator) executeSide(parent context.Context, reader datalayer.RevisionedReader, plan query.CanonicalOutline, req Request, caveatContext map[string]any) sideResult {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	iterator, err := plan.Compile()
	if err != nil {
		return sideResult{failure: &FailureRecord{Kind: ClassCompileError, Message: err.Error()}}
	}

	qctx := query.NewLocalContext(ctx,
		query.WithRevisionedReader(reader),
		query.WithCaveatRunner(caveats.NewCaveatRunner(types.Default.TypeSet)),
		query.WithCaveatContext(caveatContext),
		query.WithMaxRecursionDepth(v.cfg.recursionDepth),
	)

	switch req.Operation {
	case query.OperationCheck:
		path, err := qctx.Check(iterator, req.Resource, req.Subject)
		if err != nil {
			return sideResult{failure: classifyFailure(err)}
		}
		if path == nil {
			return sideResult{paths: nil}
		}
		return sideResult{paths: []*query.Path{path}}

	case query.OperationIterSubjects:
		// Seal the operation and target before invoking the context, matching
		// the existing wildcard semantics tests: the iterator set is observed
		// before the public API projection strips wildcard paths.
		qctx.MarkAsOperation(iterator, query.OperationIterSubjects)
		qctx.TargetSubjectType = req.SubjectFilter
		seq, err := qctx.IterSubjects(iterator, req.Resource, req.SubjectFilter)
		if err != nil {
			return sideResult{failure: classifyFailure(err)}
		}
		paths, failure := drain(ctx, seq)
		return sideResult{paths: paths, failure: failure}

	case query.OperationIterResources:
		seq, err := qctx.IterResources(iterator, req.Subject, query.NoObjectFilter())
		if err != nil {
			return sideResult{failure: classifyFailure(err)}
		}
		paths, failure := drain(ctx, seq)
		return sideResult{paths: paths, failure: failure}

	default:
		return sideResult{failure: &FailureRecord{
			Kind:    ClassExecutionError,
			Message: "planequiv: unsupported operation " + operationName(req.Operation),
		}}
	}
}
