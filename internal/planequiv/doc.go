// Package planequiv is a development-only library for finite-model regression
// validation of query plan equivalence.
//
// A PlanEquivalenceValidator executes an original and a caller-supplied
// candidate query plan against the same explicit finite models (relationship
// sets plus caveat contexts) and compares the observable semantics of Check,
// IterResources and IterSubjects on every request. The first diverging
// (model, request, context) triple is returned as a replayable, data-only
// counterexample artifact.
//
// # Scope and guarantees
//
// Equivalence here means "indistinguishable observable results on the finite
// models and requests supplied by the caller". It is explicitly NOT a
// mathematical proof that two plans are equivalent for every possible
// datastore, relationship population or caveat context, and callers must not
// present a passing report as such. The validator only exercises the local
// query executor (query.LocalExecutor) over in-process memdb snapshots; it
// never dispatches RPCs and never touches production query paths.
//
// The validator never applies optimizers itself: both plans are compiled and
// executed exactly as provided, each on its own independent query.Context with
// its own caveat runner and cancelable Go context. Running both sides through a
// shared optimizer would hide candidate-specific mistakes, so the package does
// not depend on pkg/query/queryopt at all.
//
// Artifacts are plain serializable data. They contain no functions, no
// executable expressions and no pluggable hooks; delta debugging only reruns
// the fixed internal executor with strictly smaller inputs and never executes
// artifact content.
package planequiv
