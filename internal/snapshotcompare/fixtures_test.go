package snapshotcompare_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/spicedb/internal/datastore/common"
	"github.com/authzed/spicedb/internal/datastore/memdb"
	"github.com/authzed/spicedb/internal/dispatch"
	dispatchgraph "github.com/authzed/spicedb/internal/dispatch/graph"
	"github.com/authzed/spicedb/internal/graph/computed"
	"github.com/authzed/spicedb/internal/namespace"
	"github.com/authzed/spicedb/internal/snapshotcompare"
	"github.com/authzed/spicedb/pkg/cache"
	caveattypes "github.com/authzed/spicedb/pkg/caveats/types"
	"github.com/authzed/spicedb/pkg/datalayer"
	"github.com/authzed/spicedb/pkg/datastore"
	dispatchv1 "github.com/authzed/spicedb/pkg/proto/dispatch/v1"
	"github.com/authzed/spicedb/pkg/schemadsl/compiler"
	"github.com/authzed/spicedb/pkg/schemadsl/input"
	"github.com/authzed/spicedb/pkg/tuple"
)

// leftSchema exercises arrows (group#member), an exclusion (viewer - blocked)
// and two caveats attached to the viewer relation.
const leftSchema = `
caveat cat1(condition bool) {
	condition
}

caveat cat2(allowed bool) {
	allowed
}

definition user {}

definition group {
	relation member: user
}

definition document {
	relation viewer: user | group#member | user with cat1 | user with cat2
	relation editor: user
	relation blocked: user
	permission view = viewer - blocked
	permission manage = editor + viewer
}
`

// rightSchema keeps every name but changes the cat1 caveat definition and adds
// a permission (admin) that does not exist on the left. cat2 is byte-identical.
const rightSchema = `
caveat cat1(condition bool, extra bool) {
	condition && extra == true
}

caveat cat2(allowed bool) {
	allowed
}

definition user {}

definition group {
	relation member: user
}

definition document {
	relation viewer: user | group#member | user with cat1 | user with cat2
	relation editor: user
	relation blocked: user
	permission view = viewer - blocked
	permission manage = editor + viewer
	permission admin = editor
}
`

// comparisonFixture holds a two-snapshot memdb datastore ready for comparison.
type comparisonFixture struct {
	t              *testing.T
	ds             datastore.Datastore
	dl             datalayer.DataLayer
	datastoreID    string
	dispatcher     dispatch.Dispatcher
	directDispatch dispatch.Dispatcher
	left           snapshotcompare.SnapshotRef
	right          snapshotcompare.SnapshotRef
	items          []snapshotcompare.CheckItem
}

func writeSchema(t *testing.T, ctx context.Context, dl datalayer.DataLayer, schemaText string) {
	t.Helper()
	compiled, err := compiler.Compile(compiler.InputSchema{
		Source:       input.Source("schema"),
		SchemaString: schemaText,
	}, compiler.AllowUnprefixedObjectType(),
		compiler.CaveatTypeSet(caveattypes.Default.TypeSet),
		compiler.DisallowImportFlag())
	require.NoError(t, err)

	defs := make([]datastore.SchemaDefinition, 0, len(compiled.ObjectDefinitions)+len(compiled.CaveatDefinitions))
	for _, def := range compiled.ObjectDefinitions {
		defs = append(defs, def)
	}
	for _, def := range compiled.CaveatDefinitions {
		defs = append(defs, def)
	}

	_, err = dl.ReadWriteTx(ctx, func(ctx context.Context, rwt datalayer.ReadWriteTransaction) error {
		_, err := rwt.WriteSchema(ctx, defs, schemaText, nil)
		return err
	})
	require.NoError(t, err)
}

func writeRels(t *testing.T, ctx context.Context, ds datastore.Datastore, rels ...string) {
	t.Helper()
	parsed := make([]tuple.Relationship, 0, len(rels))
	for _, rel := range rels {
		parsed = append(parsed, tuple.MustParse(rel))
	}
	_, err := common.WriteRelationships(ctx, ds, tuple.UpdateOperationCreate, parsed...)
	require.NoError(t, err)
}

func newComparisonFixture(t *testing.T) *comparisonFixture {
	t.Helper()
	ctx := t.Context()

	ds, err := memdb.NewMemdbDatastore(0, 0, memdb.DisableGC)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ds.Close() })

	// Unified-schema mode so each revision carries a real, content-addressed
	// schema hash that keys the stored-schema/compiled-caveat caches.
	dl := datalayer.NewDataLayer(ds, datalayer.WithSchemaMode(datalayer.SchemaModeReadNewWriteNew))

	writeSchema(t, ctx, dl, leftSchema)
	writeRels(t, ctx, ds,
		"document:1#viewer@user:tom",
		"document:3#viewer@group:eng",
		"group:eng#member@user:becky",
		"document:5#viewer@user:leo[cat2]",
		"document:6#viewer@user:mia[cat1]",
		"document:7#editor@user:zoe",
	)

	leftRev, leftHash, err := dl.HeadRevision(ctx)
	require.NoError(t, err)

	// Second snapshot: a same-named caveat with a changed definition, a new
	// permission, an exclusion that removes access and an added grant.
	writeSchema(t, ctx, dl, rightSchema)
	writeRels(t, ctx, ds,
		"document:1#blocked@user:tom",
		"document:2#viewer@user:jim",
	)

	rightRev, rightHash, err := dl.HeadRevision(ctx)
	require.NoError(t, err)
	require.NotEqual(t, leftHash, rightHash, "the changed schema must produce a different schema hash")

	datastoreID, err := dl.UniqueID(ctx)
	require.NoError(t, err)

	newDispatcher := func() dispatch.Dispatcher {
		params := dispatchgraph.DispatcherParameters{
			ConcurrencyLimits:      dispatchgraph.SharedConcurrencyLimits(10),
			DispatchChunkSize:      100,
			TypeSet:                caveattypes.Default.TypeSet,
			RelationshipChunkCache: cache.NoopCache[cache.StringKey, any](),
		}
		d, err := dispatchgraph.NewLocalOnlyDispatcher(params)
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		return d
	}

	items := []snapshotcompare.CheckItem{
		// 0: exclusion removes access on the right. Duplicated at position 9.
		{ResourceType: "document", Permission: "view", ResourceID: "1", SubjectType: "user", SubjectID: "tom"},
		// 1: newly granted access.
		{ResourceType: "document", Permission: "view", ResourceID: "2", SubjectType: "user", SubjectID: "jim"},
		// 2: arrow through group#member, unchanged.
		{ResourceType: "document", Permission: "view", ResourceID: "3", SubjectType: "user", SubjectID: "becky"},
		// 3: conditional via unchanged caveat cat2, provably identical.
		{ResourceType: "document", Permission: "view", ResourceID: "5", SubjectType: "user", SubjectID: "leo"},
		// 4: conditional via redefined caveat cat1, condition changed.
		{ResourceType: "document", Permission: "view", ResourceID: "6", SubjectType: "user", SubjectID: "mia"},
		// 5: request context satisfies left cat1 but not the redefined right cat1.
		{ResourceType: "document", Permission: "view", ResourceID: "6", SubjectType: "user", SubjectID: "mia",
			CaveatContext: map[string]any{"condition": true}},
		// 6: permission through an arrow union, unchanged.
		{ResourceType: "document", Permission: "manage", ResourceID: "1", SubjectType: "user", SubjectID: "tom"},
		// 7: permission exists only on the right: invalid input on the left.
		{ResourceType: "document", Permission: "admin", ResourceID: "7", SubjectType: "user", SubjectID: "zoe"},
		// 8: unknown resource type on both sides.
		{ResourceType: "widget", Permission: "view", ResourceID: "1", SubjectType: "user", SubjectID: "tom"},
		// 9: exact duplicate of position 0, retained as its own aligned row.
		{ResourceType: "document", Permission: "view", ResourceID: "1", SubjectType: "user", SubjectID: "tom"},
		// 10: plain no-permission on both sides.
		{ResourceType: "document", Permission: "view", ResourceID: "1", SubjectType: "user", SubjectID: "nia"},
	}

	return &comparisonFixture{
		t:              t,
		ds:             ds,
		dl:             dl,
		datastoreID:    datastoreID,
		dispatcher:     newDispatcher(),
		directDispatch: newDispatcher(),
		left:           snapshotcompare.SnapshotRef{Revision: leftRev, SchemaHash: leftHash},
		right:          snapshotcompare.SnapshotRef{Revision: rightRev, SchemaHash: rightHash},
		items:          items,
	}
}

func (f *comparisonFixture) newComparer(cfg snapshotcompare.Config) *snapshotcompare.Comparer {
	f.t.Helper()
	comparer, err := snapshotcompare.New(f.dl, f.dispatcher, caveattypes.Default.TypeSet, cfg)
	require.NoError(f.t, err)
	return comparer
}

func (f *comparisonFixture) params() snapshotcompare.Params {
	return snapshotcompare.Params{Left: f.left, Right: f.right, Items: f.items}
}

func caveatTypes() *caveattypes.TypeSet {
	return caveattypes.Default.TypeSet
}

// directCheck runs one Check independently of the comparison engine, mirroring
// the service-layer validation + ComputeCheck path, and returns the outcome
// that the matrix is expected to carry for this side.
func (f *comparisonFixture) directCheck(ctx context.Context, side snapshotcompare.Side, ref snapshotcompare.SnapshotRef, item snapshotcompare.CheckItem) snapshotcompare.Outcome {
	f.t.Helper()
	reader := f.dl.SnapshotReader(ref.Revision, ref.SchemaHash)
	sr, err := reader.ReadSchema(ctx)
	require.NoError(f.t, err)

	err = namespace.CheckNamespaceAndRelations(ctx, []namespace.TypeAndRelationToCheck{
		{NamespaceName: item.ResourceType, RelationName: item.Permission, AllowEllipsis: false},
		{NamespaceName: item.SubjectType, RelationName: item.SubjectRelationNormalized(), AllowEllipsis: true},
	}, sr)
	if err != nil {
		require.True(f.t, isNotFoundForTest(err), "side %s: expected not-found error, got %v", side, err)
		return snapshotcompare.OutcomeInvalidInput
	}

	result, _, err := computed.ComputeCheck(
		datalayer.ContextWithDataLayer(ctx, f.dl),
		f.directDispatch,
		caveattypes.Default.TypeSet,
		computed.CheckParameters{
			ResourceType:  tuple.RelationReference{ObjectType: item.ResourceType, Relation: item.Permission},
			Subject:       tuple.ObjectAndRelation{ObjectType: item.SubjectType, ObjectID: item.SubjectID, Relation: item.SubjectRelationNormalized()},
			CaveatContext: item.CaveatContext,
			AtRevision:    ref.Revision,
			MaximumDepth:  50,
			DebugOption:   computed.NoDebugging,
			SchemaHash:    ref.SchemaHash,
		},
		item.ResourceID,
		100,
	)
	require.NoError(f.t, err)

	switch result.GetMembership() {
	case dispatchv1.ResourceCheckResult_MEMBER:
		return snapshotcompare.OutcomeHasPermission
	case dispatchv1.ResourceCheckResult_NOT_MEMBER:
		return snapshotcompare.OutcomeNoPermission
	case dispatchv1.ResourceCheckResult_CAVEATED_MEMBER:
		return snapshotcompare.OutcomeConditional
	default:
		f.t.Fatalf("unexpected membership %v", result.GetMembership())
		return snapshotcompare.OutcomeUnspecified
	}
}
