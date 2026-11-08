package planequiv

import "fmt"

// ArtifactVersion is the explicit serialization version embedded in every
// counterexample artifact. Bump it whenever the on-disk JSON shape changes in
// an incompatible way; readers must reject unknown versions.
const ArtifactVersion = "planequiv-counterexample/v1"

// Hard ceilings cannot be relaxed through Options. They bound resource use so
// a misconfigured caller cannot turn the validator into an unbounded worker.
const (
	hardMaxModels              = 256
	hardMaxRequests            = 1024
	hardMaxRelationships       = 10000
	hardMaxContexts            = 64
	hardMaxContextFields       = 256
	hardMaxPlanDepth           = 256
	hardMaxPlanNodes           = 16384
	hardMaxArtifactBytes       = 16 << 20
	hardMaxDeltaDebugReruns    = 256
	hardMaxRecursionDepth      = 128
	hardMaxStringLength        = 64 * 1024
	defaultMaxModels           = 64
	defaultMaxRequests         = 128
	defaultMaxRelationships    = 1000
	defaultMaxContexts         = 16
	defaultMaxContextFields    = 64
	defaultMaxPlanDepth        = 64
	defaultMaxPlanNodes        = 4096
	defaultMaxArtifactBytes    = 4 << 20
	defaultMaxDeltaDebugReruns = 32
	// defaultExecutionDepth matches the query engine's own default recursion
	// budget unless the caller overrides it.
	defaultExecutionDepth = 10
)

// limits holds the effective bounds for a validator. All fields are checked
// against the hard ceilings when the validator is constructed.
type limits struct {
	maxModels           int
	maxRequests         int
	maxRelationships    int
	maxContexts         int
	maxContextFields    int
	maxPlanDepth        int
	maxPlanNodes        int
	maxArtifactBytes    int
	maxDeltaDebugReruns int
	maxRecursionDepth   int
	maxStringLength     int
}

func defaultLimits() limits {
	return limits{
		maxModels:           defaultMaxModels,
		maxRequests:         defaultMaxRequests,
		maxRelationships:    defaultMaxRelationships,
		maxContexts:         defaultMaxContexts,
		maxContextFields:    defaultMaxContextFields,
		maxPlanDepth:        defaultMaxPlanDepth,
		maxPlanNodes:        defaultMaxPlanNodes,
		maxArtifactBytes:    defaultMaxArtifactBytes,
		maxDeltaDebugReruns: defaultMaxDeltaDebugReruns,
		maxRecursionDepth:   defaultExecutionDepth,
		maxStringLength:     hardMaxStringLength,
	}
}

// boundedOption overrides a single integer limit and rejects values outside
// [1, hardCeiling].
func boundedOption(current *int, hardCeiling int, name string, value int) error {
	if value < 1 || value > hardCeiling {
		return fmt.Errorf("planequiv: %s must be between 1 and %d, got %d", name, hardCeiling, value)
	}
	*current = value
	return nil
}
