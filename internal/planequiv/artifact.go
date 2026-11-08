package planequiv

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/authzed/spicedb/pkg/query"
	"github.com/authzed/spicedb/pkg/schema/v2"
	"github.com/authzed/spicedb/pkg/tuple"
)

// scopeNote is embedded in every artifact so a replay file cannot be
// misread as a datastore-wide equivalence proof.
const scopeNote = "finite-model regression artifact: equivalence holds only for the explicit schema, relationships, contexts and requests recorded here; not a proof for arbitrary datastores or populations"

// renderOutline produces a deterministic, data-only tree description of a
// plan. The outline's own Serialize canonical key is emitted per node; the
// renderer walks the structure without compiling or executing anything.
func renderOutline(root query.Outline) string {
	var result strings.Builder
	var walk func(node query.Outline, depth int)
	walk = func(node query.Outline, depth int) {
		fmt.Fprintf(&result, "%s%s\n", strings.Repeat("  ", depth), node.Serialize())
		for _, child := range node.SubOutlines {
			walk(child, depth+1)
		}
	}
	walk(root, 0)
	return strings.TrimRight(result.String(), "\n")
}

// renderSchema returns the fixed schema text for an artifact: the original
// DSL when the caller supplied one, otherwise a deterministic rendering of
// the compiled definitions and caveats.
func renderSchema(s *schema.Schema, schemaText string) (string, error) {
	if strings.TrimSpace(schemaText) != "" {
		return schemaText, nil
	}
	defs, caveatDefs, err := s.ToDefinitions()
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, def := range defs {
		builder.WriteString(def.String())
		builder.WriteByte('\n')
	}
	for _, caveatDef := range caveatDefs {
		builder.WriteString(caveatDef.String())
		builder.WriteByte('\n')
	}
	return builder.String(), nil
}

// normalizeRelationships renders, deduplicates and sorts relationships so the
// retained input is stable across runs and safe to serialize.
func normalizeRelationships(relationships []tuple.Relationship) ([]string, error) {
	seen := make(map[string]struct{}, len(relationships))
	rendered := make([]string, 0, len(relationships))
	for _, rel := range relationships {
		text, err := tuple.String(rel)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		rendered = append(rendered, text)
	}
	sort.Strings(rendered)
	return rendered, nil
}

// sanitizeContext round-trips caveat context values through JSON. This
// guarantees the artifact carries only JSON-scalar data (no functions,
// channels or pointers to live objects) and rejects non-serializable input
// up front rather than embedding something unreadable.
func sanitizeContext(values map[string]any) (map[string]any, error) {
	if len(values) == 0 {
		return map[string]any{}, nil
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("planequiv: caveat context is not JSON-serializable: %w", err)
	}
	var sanitized map[string]any
	if err := json.Unmarshal(raw, &sanitized); err != nil {
		return nil, fmt.Errorf("planequiv: caveat context could not be sanitized: %w", err)
	}
	if sanitized == nil {
		sanitized = map[string]any{}
	}
	return sanitized, nil
}

// MarshalArtifact serializes a counterexample as versioned JSON and enforces
// the configured size ceiling. It never truncates: an oversized artifact is
// rejected so replay files cannot silently lose content.
func (v *PlanEquivalenceValidator) MarshalArtifact(c *Counterexample) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("planequiv: cannot marshal a nil counterexample")
	}
	if c.ArtifactVersion != ArtifactVersion {
		return nil, fmt.Errorf("planequiv: unsupported artifact version %q", c.ArtifactVersion)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("planequiv: counterexample is not serializable: %w", err)
	}
	if len(raw) > v.cfg.limits.maxArtifactBytes {
		return nil, fmt.Errorf("planequiv: serialized counterexample is %d bytes, exceeding the %d byte limit", len(raw), v.cfg.limits.maxArtifactBytes)
	}
	return raw, nil
}

// WriteArtifact marshals the counterexample and writes it to w in one shot.
// The artifact is data only; w receives bytes and no execution hook exists.
func (v *PlanEquivalenceValidator) WriteArtifact(c *Counterexample, w io.Writer) error {
	raw, err := v.MarshalArtifact(c)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, bytes.NewReader(raw))
	return err
}

// requestRecord renders a Request as its JSON-safe form.
func requestRecord(req Request) RequestRecord {
	record := RequestRecord{
		Operation:       operationName(req.Operation),
		ResourceType:    req.Resource.ObjectType,
		ResourceID:      req.Resource.ObjectID,
		SubjectType:     req.Subject.ObjectType,
		SubjectID:       req.Subject.ObjectID,
		SubjectRelation: req.Subject.Relation,
	}
	if req.Operation == query.OperationIterSubjects {
		record.SubjectFilter = req.SubjectFilter.String()
	}
	return record
}
