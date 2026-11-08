package snapshotcompare

// fixedRowOverhead approximates the fixed cost of a MatrixRow and its two
// SideResult values beyond their string/context payloads (enum tags, position,
// pointers, slice headers, condition metadata). It is deliberately conservative
// so result-size budgeting errs toward truncating earlier, never later.
const fixedRowOverhead = 256

// estimateRowBytes returns a conservative serialized-size estimate for a row,
// used by the MaxResultBytes budget. The estimate only needs to be monotone in
// the amount of returned data; it is not a wire-format exact size.
func estimateRowBytes(row MatrixRow) int {
	size := fixedRowOverhead
	size += len(row.Item.ResourceType) + len(row.Item.Permission) + len(row.Item.ResourceID)
	size += len(row.Item.SubjectType) + len(row.Item.SubjectID) + len(row.Item.SubjectRelation)
	size += estimateSideResultBytes(row.Left) + estimateSideResultBytes(row.Right)
	return size
}

func estimateSideResultBytes(result SideResult) int {
	size := 64
	size += len(result.InvalidReason)
	if result.Failure != nil {
		size += len(result.Failure.Error())
	}
	if result.Condition != nil {
		size += len(result.Condition.CanonicalExpression)
		size += len(result.Condition.CaveatDefinitionFingerprint)
		size += len(result.Condition.ContextSourceHash)
		for _, field := range result.Condition.MissingContext {
			size += len(field)
		}
	}
	return size
}

// overBudget reports whether a positive (non-zero) budget has been reached.
func overBudget(limit, used int) bool {
	return limit > 0 && used >= limit
}
