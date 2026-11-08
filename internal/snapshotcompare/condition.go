package snapshotcompare

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"sort"

	"google.golang.org/protobuf/proto"

	internalcaveats "github.com/authzed/spicedb/internal/caveats"
	"github.com/authzed/spicedb/pkg/datalayer"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
)

// referencedCaveatNames collects every caveat name referenced by a caveat
// expression, de-duplicated and sorted. It walks contextualized caveat leaves
// and AND/OR operation trees.
func referencedCaveatNames(expr *core.CaveatExpression) []string {
	seen := map[string]struct{}{}
	var walk func(*core.CaveatExpression)
	walk = func(e *core.CaveatExpression) {
		if e == nil {
			return
		}
		if leaf := e.GetCaveat(); leaf != nil {
			if name := leaf.GetCaveatName(); name != "" {
				seen[name] = struct{}{}
			}
			return
		}
		if op := e.GetOperation(); op != nil {
			for _, child := range op.GetChildren() {
				walk(child)
			}
		}
	}
	walk(expr)
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// caveatDefinitionFingerprint hashes the exact definitions of the caveats
// referenced by expr as visible through the given side's schema reader. The
// fingerprint changes whenever a same-named caveat's parameters or CEL body
// changes, which a name-only comparison would miss.
//
// A lookup or serialization failure does not abort the comparison: it is
// encoded into the fingerprint so the conservative result is "conditions
// differ" rather than a false "unchanged".
func caveatDefinitionFingerprint(ctx context.Context, sr datalayer.SchemaReader, expr *core.CaveatExpression) string {
	names := referencedCaveatNames(expr)
	if len(names) == 0 {
		return ""
	}
	defs, err := sr.LookupCaveatDefinitionsByNames(ctx, names)
	if err != nil {
		return "lookup-error:" + err.Error()
	}
	hasher := sha256.New()
	for _, name := range names {
		def, ok := defs[name]
		if !ok {
			// A referenced caveat missing on this side cannot be proven equal
			// to anything on the other side.
			return "missing-caveat:" + name
		}
		marshalled, err := proto.MarshalOptions{Deterministic: true}.Marshal(def)
		if err != nil {
			return "marshal-error:" + err.Error()
		}
		hasher.Write([]byte(name))
		hasher.Write([]byte{':'})
		hasher.Write([]byte(base64.RawStdEncoding.EncodeToString(marshalled)))
		hasher.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// buildConditionIdentity assembles the provable identity of a conditional
// result on one side.
func buildConditionIdentity(
	ctx context.Context,
	sr datalayer.SchemaReader,
	expr *core.CaveatExpression,
	missingFields []string,
	requestContext map[string]any,
) (*ConditionIdentity, error) {
	contextHash, err := stableContextHash(requestContext)
	if err != nil {
		return nil, err
	}
	missing := append([]string(nil), missingFields...)
	slices.Sort(missing)
	return &ConditionIdentity{
		CanonicalExpression:         internalcaveats.FromExpression(expr).String(),
		CaveatDefinitionFingerprint: caveatDefinitionFingerprint(ctx, sr, expr),
		ContextSourceHash:           contextHash,
		MissingContext:              missing,
	}, nil
}

// conditionsEqual reports whether two conditional results are provably the
// same: the normalized caveat expression must match, every referenced caveat
// definition must be byte-identical across the snapshots, the request context
// source must be the same, and the set of missing context fields must match.
// Anything that cannot be proven equal is treated as different.
func conditionsEqual(left, right *ConditionIdentity) bool {
	if left == nil || right == nil {
		return false
	}
	if left.CanonicalExpression != right.CanonicalExpression {
		return false
	}
	if left.CaveatDefinitionFingerprint != right.CaveatDefinitionFingerprint {
		return false
	}
	if left.ContextSourceHash != right.ContextSourceHash {
		return false
	}
	return slices.Equal(left.MissingContext, right.MissingContext)
}
