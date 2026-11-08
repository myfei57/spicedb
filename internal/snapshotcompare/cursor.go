package snapshotcompare

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the version of the cursor binding and comparison
// protocol. It must be bumped whenever the cursor payload or the matrix
// semantics change in an incompatible way; a cursor carrying another version
// is rejected.
const ProtocolVersion uint32 = 1

// Cursor is an opaque, resumable position token. It binds, together:
//   - the comparison protocol version,
//   - the datastore unique ID,
//   - both sides' revision strings and schema hashes,
//   - the digest of the exact checklist it was issued for, and
//   - the next unprocessed input position.
//
// A cursor never carries results or checklist contents, so it stays small
// regardless of page size.
type Cursor string

// cursorPayload is the JSON-encoded cursor body.
type cursorPayload struct {
	ProtocolVersion uint32 `json:"protocol_version"`
	DatastoreID     string `json:"datastore_id"`
	LeftRevision    string `json:"left_revision"`
	LeftSchemaHash  string `json:"left_schema_hash"`
	RightRevision   string `json:"right_revision"`
	RightSchemaHash string `json:"right_schema_hash"`
	ChecklistDigest string `json:"checklist_digest"`
	NextPosition    int    `json:"next_position"`
}

// EncodeCursor builds an opaque cursor for the given binding. Callers normally
// obtain cursors from a page footer; this constructor is exposed so that
// tooling which persists cursors can rebuild them from recorded bindings.
func EncodeCursor(datastoreID string, left, right SnapshotRef, checklistDigest string, nextPosition int) (Cursor, error) {
	if left.Revision == nil || right.Revision == nil {
		return "", fmt.Errorf("%w: missing revision in cursor binding", ErrCursorInvalid)
	}
	payload := cursorPayload{
		ProtocolVersion: ProtocolVersion,
		DatastoreID:     datastoreID,
		LeftRevision:    left.Revision.String(),
		LeftSchemaHash:  string(left.SchemaHash),
		RightRevision:   right.Revision.String(),
		RightSchemaHash: string(right.SchemaHash),
		ChecklistDigest: checklistDigest,
		NextPosition:    nextPosition,
	}
	marshalled, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrCursorInvalid, err)
	}
	return Cursor(base64.RawURLEncoding.EncodeToString(marshalled)), nil
}

// decodeCursor parses an opaque cursor. Structural and protocol failures are
// wrapped in ErrCursorInvalid; binding checks against the current request
// happen in the engine.
func decodeCursor(token Cursor) (cursorPayload, error) {
	var payload cursorPayload
	raw, err := base64.RawURLEncoding.DecodeString(string(token))
	if err != nil {
		return payload, fmt.Errorf("%w: token is not base64: %w", ErrCursorInvalid, err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("%w: token payload is malformed: %w", ErrCursorInvalid, err)
	}
	if payload.ProtocolVersion != ProtocolVersion {
		return payload, fmt.Errorf("%w: protocol version %d is not supported (expected %d)",
			ErrCursorInvalid, payload.ProtocolVersion, ProtocolVersion)
	}
	if payload.NextPosition < 0 {
		return payload, fmt.Errorf("%w: negative next position", ErrCursorInvalid)
	}
	return payload, nil
}
