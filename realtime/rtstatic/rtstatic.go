// Package rtstatic embeds and verifies the static key inside GTFS-Realtime
// feed messages.
//
// GTFS-Realtime v3.0 defines feed_header.feed_version (proto field 4) so
// consumers can verify a realtime proto against the static dataset it was
// generated from. The MobilityData Go bindings used by this project are still
// at v1.0.0 (gtfs_realtime_version "2.0"), whose FeedHeader struct has no
// FeedVersion field. To stay on the pinned binding version while emitting the
// standard wire field, the key is written into the header's unknown fields
// with the exact v3.0 wire encoding:
//
//	field 4, wire type 2 (length-delimited string).
//
// Protobuf's forward-compatibility guarantees that v3-aware consumers read it
// as feed_header.feed_version; v2 consumers simply skip the unknown field.
// On re-marshal with the v2 bindings unknown fields are preserved byte-for-byte
// (see TestKeySurvivesMarshalRoundTrip).
package rtstatic

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// FeedVersionFieldNumber is the proto tag number of
// FeedHeader.feed_version in the GTFS-Realtime v3.0 schema.
const FeedVersionFieldNumber protowire.Number = 4

// ErrMissingKey is returned by StaticKey/Verify when the feed header carries
// no static key field.
var ErrMissingKey = errors.New("GTFS-RT feed header has no static key")

// appendFieldString appends a length-delimited string field (wire type 2)
// with the given field number to b.
func appendFieldString(b []byte, num protowire.Number, v string) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	b = protowire.AppendString(b, v)
	return b
}

// stripField removes every occurrence of the given length-delimited field from
// a raw protobuf payload, preserving the order and bytes of all other fields.
func stripField(b []byte, num protowire.Number) []byte {
	out := b[:0:0]
	for len(b) > 0 {
		fieldNum, wireType, n := protowire.ConsumeTag(b)
		if n < 0 {
			// Malformed tail: keep the remainder untouched rather than panic.
			return append(out, b...)
		}
		tagEnd := n
		fieldLen := protowire.ConsumeFieldValue(fieldNum, wireType, b[tagEnd:])
		if fieldLen < 0 {
			return append(out, b...)
		}
		total := tagEnd + fieldLen
		if fieldNum != num {
			out = append(out, b[:total]...)
		}
		b = b[total:]
	}
	return out
}

// SetStaticKey stamps the static key into the feed header as the standard
// GTFS-RT v3.0 feed_version field (field 4). Any previously stored value is
// replaced. The message must already carry a non-nil Header.
func SetStaticKey(msg *gtfsrt.FeedMessage, key string) error {
	if msg == nil || msg.Header == nil {
		return errors.New("cannot set static key on a feed message without a header")
	}
	if key == "" {
		return errors.New("static key must not be empty")
	}

	header := msg.Header.ProtoReflect()
	raw := stripField(header.GetUnknown(), FeedVersionFieldNumber)
	raw = appendFieldString(raw, FeedVersionFieldNumber, key)
	header.SetUnknown(raw)
	return nil
}

// extractFieldString returns the last value of a length-delimited field in a
// raw protobuf payload.
func extractFieldString(b []byte, num protowire.Number) (string, bool) {
	found := false
	value := ""
	for len(b) > 0 {
		fieldNum, wireType, tagLen := protowire.ConsumeTag(b)
		if tagLen < 0 {
			return value, found
		}
		// ConsumeFieldValue returns the value size including its length
		// prefix varint for length-delimited fields.
		fieldLen := protowire.ConsumeFieldValue(fieldNum, wireType, b[tagLen:])
		if fieldLen < 0 {
			return value, found
		}
		if fieldNum == num && wireType == protowire.BytesType {
			payload, m := protowire.ConsumeBytes(b[tagLen : tagLen+fieldLen])
			if m < 0 {
				return "", false
			}
			value = string(payload)
			found = true
		}
		b = b[tagLen+fieldLen:]
	}
	return value, found
}

// StaticKey reads the static key stamped into the feed header as the v3.0
// feed_version field (field 4). ErrMissingKey is returned when no key is
// present, so callers can distinguish an unsigned feed from a parse failure.
func StaticKey(msg *gtfsrt.FeedMessage) (string, error) {
	if msg == nil || msg.Header == nil {
		return "", ErrMissingKey
	}
	raw := msg.Header.ProtoReflect().GetUnknown()
	if key, ok := extractFieldString(raw, FeedVersionFieldNumber); ok && key != "" {
		return key, nil
	}
	return "", ErrMissingKey
}

// Marshal works like proto.Marshal but guarantees the static key is embedded
// in the serialized bytes. Set StaticKey first, or pass a non-empty key here
// to stamp it during marshalling.
func Marshal(msg *gtfsrt.FeedMessage, key string) ([]byte, error) {
	if key != "" {
		if err := SetStaticKey(msg, key); err != nil {
			return nil, err
		}
	}
	return proto.Marshal(msg)
}

// MarshalReadable renders msg in the human-readable prototext format.
//
// prototext deliberately omits unknown fields, so the v3 feed_version (field
// 4, unknown to the v1.0.0 bindings) is appended explicitly as
// `4: "<key>"` inside the header block. The emitted text parses back to the
// same bytes as the binary form (see TestWriteFileAtomicReadable).
func MarshalReadable(msg *gtfsrt.FeedMessage, key string) ([]byte, error) {
	if key != "" {
		if err := SetStaticKey(msg, key); err != nil {
			return nil, err
		}
	}
	b, err := prototext.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return b, nil
	}
	// Insert the unknown-field rendering right after "header:{", so it lives
	// inside the header block rather than at the top level.
	insert := []byte(fmt.Sprintf("header:{ 4: %q ", key))
	return bytes.Replace(b, []byte("header:{"), insert, 1), nil
}

// Unmarshal parses a GTFS-RT feed message, including any v3 feed_version field
// carried as an unknown field by the v1.0.0 bindings.
func Unmarshal(b []byte) (*gtfsrt.FeedMessage, error) {
	msg := &gtfsrt.FeedMessage{}
	if err := proto.Unmarshal(b, msg); err != nil {
		return nil, fmt.Errorf("unmarshalling GTFS-RT feed message: %w", err)
	}
	return msg, nil
}

// Verify unmarshals b and asserts that its embedded static key equals wantKey.
// It returns the parsed message and the key found in the header.
func Verify(b []byte, wantKey string) (*gtfsrt.FeedMessage, string, error) {
	msg, err := Unmarshal(b)
	if err != nil {
		return nil, "", err
	}
	gotKey, err := StaticKey(msg)
	if err != nil {
		return msg, "", err
	}
	if gotKey != wantKey {
		return msg, gotKey, fmt.Errorf(
			"static key mismatch: proto built for %q, expected %q", gotKey, wantKey)
	}
	return msg, gotKey, nil
}

// WriteFileAtomic serializes msg to target via a temp file + rename, so
// consumers never read a half-written proto. When key is non-empty it is
// stamped into the header before marshalling. humanReadable selects the
// prototext representation instead of the binary wire format.
func WriteFileAtomic(target string, msg *gtfsrt.FeedMessage, key string, humanReadable bool) error {
	var b []byte
	var err error
	if humanReadable {
		b, err = MarshalReadable(msg, key)
	} else {
		b, err = Marshal(msg, key)
	}
	if err != nil {
		return err
	}

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*.pb")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}
