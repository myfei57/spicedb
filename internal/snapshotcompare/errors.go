package snapshotcompare

import "errors"

// Sentinel errors returned by the comparison engine. Callers should use
// errors.Is rather than matching message text.
var (
	// ErrEmptyChecklist is returned when a comparison is requested without any
	// checklist entries.
	ErrEmptyChecklist = errors.New("snapshot comparison checklist is empty")

	// ErrInvalidConfig is returned when the Comparer configuration or
	// comparison parameters are invalid.
	ErrInvalidConfig = errors.New("snapshot comparison configuration is invalid")

	// ErrCursorInvalid is the wrapped parent of every cursor rejection. Mismatch
	// causes wrap it so errors.Is(err, ErrCursorInvalid) always identifies a
	// stale or incompatible resume token.
	ErrCursorInvalid = errors.New("snapshot comparison cursor is invalid")

	// ErrRevisionUnavailable wraps a datastore invalid/expired revision error
	// raised while establishing or resuming a comparison.
	ErrRevisionUnavailable = errors.New("snapshot comparison revision is unavailable")

	// ErrContextCanceled is returned when the shared context is canceled or
	// times out before the page completes. No partial page is returned.
	ErrContextCanceled = errors.New("snapshot comparison canceled")
)
