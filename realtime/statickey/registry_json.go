package statickey

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// registryJSON is the on-disk serialization shape of Registry.
type registryJSON struct {
	Version int           `json:"version"`
	Pairs   []VersionPair `json:"pairs"`
}

// registryFormatVersion is bumped if the serialization shape changes.
const registryFormatVersion = 1

// marshalRegistry serializes pairs into the versions.json payload.
func marshalRegistry(pairs []VersionPair) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(registryJSON{Version: registryFormatVersion, Pairs: pairs}); err != nil {
		return nil, fmt.Errorf("marshalling version registry: %w", err)
	}
	return buf.Bytes(), nil
}

// unmarshalRegistry parses a versions.json payload into r.
func unmarshalRegistry(b []byte, r *Registry) error {
	var doc registryJSON
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parsing version registry: %w", err)
	}
	if doc.Version != registryFormatVersion {
		return fmt.Errorf(
			"unsupported version registry format: got %d, want %d",
			doc.Version, registryFormatVersion)
	}

	// Normalize timestamps to UTC and validate minimal shape.
	for i, p := range doc.Pairs {
		if p.Static.Key == "" || p.Static.SHA256 == "" {
			return fmt.Errorf("version registry pair %d is missing key or sha256", i)
		}
		doc.Pairs[i].Static.PublishedAt = p.Static.PublishedAt.UTC()
	}
	r.Pairs = doc.Pairs
	return nil
}
