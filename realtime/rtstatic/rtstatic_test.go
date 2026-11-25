package rtstatic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gtfsrt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func sampleMessage() *gtfsrt.FeedMessage {
	ver := "2.0"
	incr := gtfsrt.FeedHeader_FULL_DATASET
	ts := uint64(1700000000)
	id := "V/1/001"
	return &gtfsrt.FeedMessage{
		Header: &gtfsrt.FeedHeader{
			GtfsRealtimeVersion: &ver,
			Incrementality:      &incr,
			Timestamp:           &ts,
		},
		Entity: []*gtfsrt.FeedEntity{{Id: &id}},
	}
}

func TestSetAndReadStaticKey(t *testing.T) {
	msg := sampleMessage()
	if err := SetStaticKey(msg, "2026-11-25 03:00:00"); err != nil {
		t.Fatalf("SetStaticKey: %v", err)
	}

	got, err := StaticKey(msg)
	if err != nil {
		t.Fatalf("StaticKey: %v", err)
	}
	if got != "2026-11-25 03:00:00" {
		t.Fatalf("got %q", got)
	}
}

func TestStaticKeyMissing(t *testing.T) {
	if _, err := StaticKey(sampleMessage()); err != ErrMissingKey {
		t.Fatalf("got %v, want ErrMissingKey", err)
	}
}

// TestKeySurvivesMarshalRoundTrip is the core compatibility guarantee:
// although the v1.0.0 Go bindings don't know field 4, the unknown field must
// survive marshal/unmarshal so v3-aware consumers see feed_header.feed_version.
func TestKeySurvivesMarshalRoundTrip(t *testing.T) {
	msg := sampleMessage()
	key := "2026-11-25 03:00:00"

	b, err := Marshal(msg, key)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// field 4 lives inside the header submessage (FeedMessage field 1):
	// peel the header bytes off first, then assert the v3 field value.
	headerBytes := extractHeaderBytes(t, b)
	if got, ok := findStringField(headerBytes, FeedVersionFieldNumber); !ok || got != key {
		t.Fatalf("feed_version field 4 on the wire = %q (present=%v), want %q; bytes=%x",
			got, ok, key, b)
	}

	parsed, err := Unmarshal(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, err := StaticKey(parsed)
	if err != nil {
		t.Fatalf("StaticKey after round trip: %v", err)
	}
	if got != key {
		t.Fatalf("round-trip key = %q, want %q", got, key)
	}

	// Standard library re-marshal of the parsed message must keep the field.
	reMarshaled, err := proto.Marshal(parsed)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	reParsed, err := Unmarshal(reMarshaled)
	if err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	got2, _ := StaticKey(reParsed)
	if got2 != key {
		t.Fatalf("key lost after second marshal: got %q", got2)
	}
}

func TestSetStaticKeyReplacesOldValue(t *testing.T) {
	msg := sampleMessage()
	if err := SetStaticKey(msg, "old"); err != nil {
		t.Fatal(err)
	}
	if err := SetStaticKey(msg, "new"); err != nil {
		t.Fatal(err)
	}
	got, _ := StaticKey(msg)
	if got != "new" {
		t.Fatalf("got %q, want new", got)
	}

	// Exactly one field-4 occurrence must exist inside the header.
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	headerBytes := extractHeaderBytes(t, b)
	if count := countFields(headerBytes, FeedVersionFieldNumber); count != 1 {
		t.Fatalf("got %d field-4 tags, want 1; header=%x", count, headerBytes)
	}
}

// findStringField walks a raw message and returns the last string value of
// the requested field number.
func findStringField(b []byte, num protowire.Number) (string, bool) {
	found, value := false, ""
	for len(b) > 0 {
		fieldNum, wireType, tagLen := protowire.ConsumeTag(b)
		if tagLen < 0 {
			return value, found
		}
		// ConsumeFieldValue includes the length-prefix varint in its result.
		fieldLen := protowire.ConsumeFieldValue(fieldNum, wireType, b[tagLen:])
		if fieldLen < 0 {
			return value, found
		}
		if fieldNum == num && wireType == protowire.BytesType {
			payload, m := protowire.ConsumeBytes(b[tagLen : tagLen+fieldLen])
			if m < 0 {
				return "", false
			}
			value, found = string(payload), true
		}
		b = b[tagLen+fieldLen:]
	}
	return value, found
}

// countFields returns how many times num appears in a raw message.
func countFields(b []byte, num protowire.Number) int {
	count := 0
	for len(b) > 0 {
		fieldNum, wireType, tagLen := protowire.ConsumeTag(b)
		if tagLen < 0 {
			return count
		}
		fieldLen := protowire.ConsumeFieldValue(fieldNum, wireType, b[tagLen:])
		if fieldLen < 0 {
			return count
		}
		if fieldNum == num {
			count++
		}
		b = b[tagLen+fieldLen:]
	}
	return count
}

// extractHeaderBytes peels the FeedHeader submessage (FeedMessage field 1)
// out of a marshalled FeedMessage.
func extractHeaderBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	fieldNum, wireType, tagLen := protowire.ConsumeTag(b)
	if tagLen < 0 || fieldNum != 1 || wireType != protowire.BytesType {
		t.Fatalf("expected header field 1, got num=%d type=%d", fieldNum, wireType)
	}
	fieldLen := protowire.ConsumeFieldValue(fieldNum, wireType, b[tagLen:])
	if fieldLen < 0 {
		t.Fatal("malformed header length")
	}
	payload, m := protowire.ConsumeBytes(b[tagLen : tagLen+fieldLen])
	if m < 0 {
		t.Fatal("malformed header payload")
	}
	return payload
}

func TestVerify(t *testing.T) {
	msg := sampleMessage()
	b, err := Marshal(msg, "key-A")
	if err != nil {
		t.Fatal(err)
	}

	if _, got, err := Verify(b, "key-A"); err != nil || got != "key-A" {
		t.Fatalf("Verify matching key failed: %v %q", err, got)
	}
	if _, _, err := Verify(b, "key-B"); err == nil {
		t.Fatal("Verify must reject a mismatching key")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "positions.pb")

	msg := sampleMessage()
	if err := WriteFileAtomic(target, msg, "key-file", false); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if _, _, err := Verify(b, "key-file"); err != nil {
		t.Fatalf("written file fails verification: %v", err)
	}

	// No temp files may be left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".pb" && e.Name() != "positions.pb" {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWriteFileAtomicReadable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "positions.txtpb")
	msg := sampleMessage()
	if err := WriteFileAtomic(target, msg, "key-text", true); err != nil {
		t.Fatalf("WriteFileAtomic readable: %v", err)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	// prototext renders unknown fields with their raw number: 4: "key-text"
	if !strings.Contains(string(b), `4: "key-text"`) {
		t.Fatalf("human-readable output missing key: %s", b)
	}
}

func TestSetStaticKeyValidation(t *testing.T) {
	if err := SetStaticKey(nil, "x"); err == nil {
		t.Fatal("nil message must fail")
	}
	if err := SetStaticKey(&gtfsrt.FeedMessage{}, "x"); err == nil {
		t.Fatal("header-less message must fail")
	}
	if err := SetStaticKey(sampleMessage(), ""); err == nil {
		t.Fatal("empty key must fail")
	}
}

// TestStaticKeyLongStringRegression guards against the length-prefix bug:
// with a key of 128+ bytes the varint length spans two bytes, which the old
// ConsumeString-based implementation misread as part of the string.
func TestStaticKeyLongStringRegression(t *testing.T) {
	key := strings.Repeat("0123456789", 20) // 200 bytes: length varint spans 2 bytes
	msg := sampleMessage()
	if err := SetStaticKey(msg, key); err != nil {
		t.Fatal(err)
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := StaticKey(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if got != key {
		t.Fatalf("long key round trip mismatch: got %d bytes, want %d", len(got), len(key))
	}
}
