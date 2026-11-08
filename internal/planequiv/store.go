package planequiv

import (
	"context"
	"errors"
	"time"

	"github.com/authzed/spicedb/internal/datastore/memdb"
	"github.com/authzed/spicedb/pkg/datalayer"
	"github.com/authzed/spicedb/pkg/datastore"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
	"github.com/authzed/spicedb/pkg/schema/v2"
	"github.com/authzed/spicedb/pkg/tuple"
)

// modelStore is one independently built, in-process interpretation of a finite
// model. Both plan sides execute read-only against the same revision, but each
// gets its own query.Context. Closing releases the memdb instance.
type modelStore struct {
	dl     datalayer.DataLayer
	reader datalayer.RevisionedReader
}

// close releases the underlying datastore. It is safe to call once per store.
func (m *modelStore) close() {
	if m != nil && m.dl != nil {
		_ = m.dl.Close()
	}
}

// buildModelStore creates a fresh memdb datastore, writes the fixed schema
// (including caveat definitions) and exactly the given relationships, and
// returns a snapshot reader at the resulting revision. Every invocation is
// independent, which keeps reruns (including delta-debugging reruns) hermetic.
func buildModelStore(ctx context.Context, s *schema.Schema, caveatDefs []*core.CaveatDefinition, relationships []tuple.Relationship) (*modelStore, error) {
	if s == nil {
		return nil, errors.New("planequiv: nil schema")
	}
	ds, err := memdb.NewMemdbDatastore(0, time.Second, memdb.DisableGC)
	if err != nil {
		return nil, err
	}

	defs, schemaCaveats, err := s.ToDefinitions()
	if err != nil {
		_ = ds.Close()
		return nil, err
	}
	// Schema.ToDefinitions drops caveat parameter types, so prefer the
	// caller-retained definitions whenever the schema declares caveats.
	if len(s.Caveats()) > 0 {
		if len(caveatDefs) != len(s.Caveats()) {
			_ = ds.Close()
			return nil, errors.New("planequiv: caveated schema requires the original CaveatDefinitions retaining parameter types")
		}
		schemaCaveats = caveatDefs
	}

	revision, err := ds.ReadWriteTx(ctx, func(ctx context.Context, tx datastore.ReadWriteTransaction) error {
		if err := tx.LegacyWriteNamespaces(ctx, defs...); err != nil {
			return err
		}
		if len(schemaCaveats) > 0 {
			if err := tx.LegacyWriteCaveats(ctx, schemaCaveats); err != nil {
				return err
			}
		}
		updates := make([]tuple.RelationshipUpdate, 0, len(relationships))
		for _, rel := range relationships {
			updates = append(updates, tuple.Touch(rel))
		}
		return tx.WriteRelationships(ctx, updates)
	})
	if err != nil {
		_ = ds.Close()
		return nil, err
	}

	dl := datalayer.NewDataLayer(ds)
	return &modelStore{
		dl:     dl,
		reader: dl.SnapshotReader(revision, datalayer.NoSchemaHashForTesting),
	}, nil
}
