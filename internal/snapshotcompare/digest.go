package snapshotcompare

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	pkgcaveats "github.com/authzed/spicedb/pkg/caveats"
)

// canonicalItem is the fixed-field-order JSON shape used both for the
// within-page dedup key and for the checklist digest. Go's encoding/json
// already emits map keys in sorted order, so marshaling this struct is
// deterministic for equal semantic inputs regardless of map iteration order.
type canonicalItem struct {
	ResourceType    string         `json:"resource_type"`
	Permission      string         `json:"permission"`
	ResourceID      string         `json:"resource_id"`
	SubjectType     string         `json:"subject_type"`
	SubjectID       string         `json:"subject_id"`
	SubjectRelation string         `json:"subject_relation"`
	CaveatContext   map[string]any `json:"caveat_context,omitempty"`
}

func canonicalItemOf(item CheckItem) canonicalItem {
	return canonicalItem{
		ResourceType:    item.ResourceType,
		Permission:      item.Permission,
		ResourceID:      item.ResourceID,
		SubjectType:     item.SubjectType,
		SubjectID:       item.SubjectID,
		SubjectRelation: item.SubjectRelationNormalized(),
		CaveatContext:   item.CaveatContext,
	}
}

// canonicalKey returns a stable string identifying semantically equal inputs.
// It is the dedup key: entries with the same key execute once per side within
// a page while every entry still gets its own aligned row.
func canonicalKey(item CheckItem) (string, error) {
	encoded, err := json.Marshal(canonicalItemOf(item))
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// ChecklistDigest is the hex-encoded SHA-256 digest of the canonical encoding
// of every checklist entry in input order. It is bound into every cursor, so a
// cursor can only resume the exact checklist it was created for.
func ChecklistDigest(items []CheckItem) (string, error) {
	hasher := sha256.New()
	var lengthPrefix [8]byte
	for _, item := range items {
		encoded, err := json.Marshal(canonicalItemOf(item))
		if err != nil {
			return "", err
		}
		// Length-prefix each entry so the framing cannot confuse two adjacent
		// entries for one.
		binary.BigEndian.PutUint64(lengthPrefix[:], uint64(len(encoded)))
		if _, err := hasher.Write(lengthPrefix[:]); err != nil {
			return "", err
		}
		if _, err := hasher.Write(encoded); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// stableContextHash returns a stable, content-addressed string for the
// request-supplied caveat context. An empty context hashes to the empty
// string, which is itself a provable "same source" value.
func stableContextHash(caveatContext map[string]any) (string, error) {
	if len(caveatContext) == 0 {
		return "", nil
	}
	structCtx, err := structpb.NewStruct(caveatContext)
	if err != nil {
		return "", err
	}
	return pkgcaveats.StableContextStringForHashing(structCtx)
}

// contextByteSize returns the serialized protobuf size of the request caveat
// context, matching the byte accounting the API layer applies, and zero for
// an empty context.
func contextByteSize(caveatContext map[string]any) (int, error) {
	if len(caveatContext) == 0 {
		return 0, nil
	}
	structCtx, err := structpb.NewStruct(caveatContext)
	if err != nil {
		return 0, err
	}
	return proto.Size(structCtx), nil
}
