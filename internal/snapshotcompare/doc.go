// Package snapshotcompare implements PermissionSnapshotComparison: a finite,
// read-only comparison of Check behavior between two explicitly pinned
// datastore snapshots.
//
// Each side is pinned to its own (revision, schema hash) pair. Checks are
// executed through the supplied dispatcher with the side's own revision and
// schema hash on every request, so a schema version visible only on one side
// can never be served from the other side's stored-schema (and compiled
// caveat) cache: the datalayer keys stored schemas by schema hash, and the
// derived compiled-caveat cache lives on the stored schema of that exact
// version.
//
// # Scope and soundness
//
// A comparison only covers the finite, caller-supplied checklist. Its matrix
// describes how those exact inputs behave on the two snapshots and must never
// be presented as a proof of full permission-set equivalence. Every returned
// footer carries ScopeStatement to that effect.
//
// The five per-side outcomes (no permission, has permission, conditional,
// invalid input on that side's schema, execution failure) are represented
// separately. Unknown or unevaluable inputs are never folded into a grant or
// denial: rows touching invalid inputs, failures, or differing conditions are
// reported as ConditionChanged or Incomparable rather than guessed.
//
// The package never writes schema or relationships: it only uses snapshot
// readers, revision checks and the dispatcher's Check path.
package snapshotcompare
