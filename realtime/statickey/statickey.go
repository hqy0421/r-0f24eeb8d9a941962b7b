// Package statickey handles identification of static GTFS datasets and the
// version-pair registry used during static/GTFS-Realtime handovers.
//
// A "static key" identifies the exact static GTFS ZIP a GTFS-Realtime proto
// was built against. It is derived from the feed_version declared in
// feed_info.txt (falling back to a SHA-256 content hash when feed_info.txt or
// its feed_version column is missing), e.g.
//
//	2026-11-25 03:00:00
//	sha256:9f1c...
//
// The registry (versions.json) keeps the most recent version *pairs*. Every
// entry pairs one static dataset (key + hash + local path) with the GTFS-RT
// protos generated from it. At most two pairs are retained, so the previous,
// still-readable pair survives a failed handover: a new pair is only committed
// after its proto has been successfully constructed, and the static file is
// swapped in atomically. If anything fails in between, the registry and the
// previous pair stay untouched.
package statickey

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxPairs is the number of (static, realtime) version pairs retained in the
// registry: the current pair and the previous one.
const maxPairs = 2

// FeedInfoName is the name of the GTFS file carrying the declared feed version.
const FeedInfoName = "feed_info.txt"

// HashPrefix marks a key derived from a content hash instead of a declared
// feed_version.
const HashPrefix = "sha256:"

// ErrNoFeedVersion is returned when feed_info.txt exists but contains no
// usable feed_version value.
var ErrNoFeedVersion = errors.New("feed_info.txt has no usable feed_version column")

// StaticVersion describes one published static GTFS dataset.
type StaticVersion struct {
	// Key is the static key: the declared feed_version, or
	// "sha256:<hex>" when the feed does not declare one.
	Key string `json:"key"`

	// FeedVersion is the raw value of feed_info.txt's feed_version column.
	// Empty when the feed does not declare a version.
	FeedVersion string `json:"feed_version"`

	// SHA256 is the lowercase hex SHA-256 digest of the ZIP bytes.
	SHA256 string `json:"sha256"`

	// Path is the static ZIP location inside the version store
	// (e.g. "static/2026-11-25_030000.zip").
	Path string `json:"path"`

	// PublishedAt is when the pair was committed to the registry (UTC).
	PublishedAt time.Time `json:"published_at"`
}

// VersionPair pairs one static dataset with the GTFS-Realtime protos built
// from it. Both alerts and positions feeds may be empty when that feed type is
// not produced by this deployment.
type VersionPair struct {
	Static StaticVersion `json:"static"`
	// Alerts is the alerts GTFS-RT proto path relative to the store root.
	Alerts string `json:"alerts,omitempty"`
	// Positions is the vehicle-positions GTFS-RT proto path relative to
	// the store root.
	Positions string `json:"positions,omitempty"`
}

// Registry is the on-disk collection of version pairs.
type Registry struct {
	// root is the directory holding versions.json and the per-version files.
	root string

	// Pairs are ordered newest-first (index 0 is the current pair).
	Pairs []VersionPair `json:"pairs"`
}

// StorePath joins the registry root with a store-relative path.
func StorePath(root, rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }

// registryPath is the location of versions.json inside a store root.
func registryPath(root string) string { return filepath.Join(root, "versions.json") }

/* -------------------------
   static key computation
  ------------------------- */

// HashBytes returns the lowercase hex SHA-256 digest of the given bytes.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashReader streams r to completion and returns its lowercase hex SHA-256.
func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashFile returns the lowercase hex SHA-256 of the file at path.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return HashReader(f)
}

// ReadFeedVersion extracts the first non-empty feed_version value from a
// feed_info.txt CSV stream. Returns ErrNoFeedVersion if the column is absent
// or every value is empty.
func ReadFeedVersion(r io.Reader) (string, error) {
	return readFeedVersion(r)
}

// readFeedVersion extracts the first non-empty feed_version value from a
// feed_info.txt CSV stream. Returns ErrNoFeedVersion if the column is absent
// or every value is empty.
func readFeedVersion(r io.Reader) (string, error) {
	csvReader := csv.NewReader(r)
	csvReader.FieldsPerRecord = -1 // tolerate ragged rows

	header, err := csvReader.Read()
	if err != nil {
		return "", fmt.Errorf("reading feed_info.txt header: %w", err)
	}

	col := -1
	for i, name := range header {
		if name == "feed_version" {
			col = i
			break
		}
	}
	if col < 0 {
		return "", ErrNoFeedVersion
	}

	for {
		row, err := csvReader.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			return "", fmt.Errorf("reading feed_info.txt rows: %w", err)
		}
		if col < len(row) && row[col] != "" {
			return row[col], nil
		}
	}
	return "", ErrNoFeedVersion
}

// zipFileReader opens the named entry inside the ZIP.
func zipFileReader(zr *zip.Reader, name string) (io.ReadCloser, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f.Open()
		}
	}
	return nil, fmt.Errorf("file %s not found in GTFS archive", name)
}

// KeyFromZip computes the static key for an in-memory ZIP reader: the declared
// feed_version if present, otherwise HashPrefix + the provided content hash.
// A missing feed_info.txt or feed_version column is not an error - the hash
// fallback is used.
func KeyFromZip(zr *zip.Reader, contentHash string) (key, feedVersion string, err error) {
	rc, openErr := zipFileReader(zr, FeedInfoName)
	if openErr != nil {
		// feed_info.txt is not part of the archive - use the hash fallback.
		return HashPrefix + contentHash, "", nil
	}
	defer rc.Close()

	feedVersion, readErr := ReadFeedVersion(rc)
	if readErr != nil || feedVersion == "" {
		// The archive is present but carries no usable feed_version.
		return HashPrefix + contentHash, "", nil
	}
	return feedVersion, feedVersion, nil
}

// DescribeFile hashes the ZIP at path and resolves its static key. It does not
// modify the file.
func DescribeFile(path string) (StaticVersion, error) {
	hash, err := HashFile(path)
	if err != nil {
		return StaticVersion{}, err
	}

	f, err := os.Open(path)
	if err != nil {
		return StaticVersion{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return StaticVersion{}, err
	}

	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return StaticVersion{}, fmt.Errorf("opening GTFS archive %s: %w", path, err)
	}

	key, feedVersion, err := KeyFromZip(zr, hash)
	if err != nil {
		return StaticVersion{}, err
	}

	return StaticVersion{
		Key:         key,
		FeedVersion: feedVersion,
		SHA256:      hash,
	}, nil
}

/* -------------------------
   registry persistence
  ------------------------- */

// LoadRegistry reads versions.json from root. A missing registry file yields
// an empty registry rooted at root (so first-time publication works).
func LoadRegistry(root string) (*Registry, error) {
	r := &Registry{root: root}
	b, err := os.ReadFile(registryPath(root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, nil
		}
		return nil, err
	}
	if err := unmarshalRegistry(b, r); err != nil {
		return nil, err
	}
	r.root = root
	return r, nil
}

// Root returns the store root directory.
func (r *Registry) Root() string { return r.root }

// Current returns the newest pair and true, or a zero pair and false when the
// registry is empty.
func (r *Registry) Current() (VersionPair, bool) {
	if len(r.Pairs) == 0 {
		return VersionPair{}, false
	}
	return r.Pairs[0], true
}

// FindByKey returns the pair whose static key matches, if still retained.
func (r *Registry) FindByKey(key string) (VersionPair, bool) {
	for _, p := range r.Pairs {
		if p.Static.Key == key {
			return p, true
		}
	}
	return VersionPair{}, false
}

// AbsPath resolves a store-relative file path against the registry root.
func (r *Registry) AbsPath(rel string) string { return StorePath(r.root, rel) }

// ensureDir creates parent directories for a store-relative path.
func (r *Registry) ensureDir(rel string) error {
	dir := filepath.Dir(r.AbsPath(rel))
	return os.MkdirAll(dir, 0o777)
}

// stageFile copies src into the store under rel using a temp file + rename so
// readers never observe a partially written file.
func (r *Registry) stageFile(rel, src string) error {
	if err := r.ensureDir(rel); err != nil {
		return err
	}
	dst := r.AbsPath(rel)

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := io.Copy(tmp, in); err != nil {
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
	return os.Rename(tmpName, dst)
}

// sanitizeName makes a file/directory-safe slug from a static key.
func sanitizeName(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == ':' || r == '/':
			b.WriteRune('_')
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// pairDir is the per-pair directory inside the store, e.g. "versions/2026-11-25_030000".
func pairDir(sv StaticVersion) string {
	return filepath.ToSlash(filepath.Join("versions", sanitizeName(sv.Key)))
}

// CommitOptions carries the GTFS-RT proto files (absolute paths) produced from
// a candidate static ZIP. Empty fields mean that feed type is not produced.
type CommitOptions struct {
	AlertsProto    string
	PositionsProto string
}

// Commit publishes a new version pair atomically:
//
//  1. The candidate ZIP is copied (temp+rename) into a new per-pair directory.
//  2. The successfully-built protos are copied next to it.
//  3. versions.json is rewritten (temp+rename) with the new pair first,
//     trimming to the two newest pairs; files belonging to evicted pairs are
//     removed only after the new registry is durable.
//
// A pair with the same static key already present is returned as-is, making
// Commit idempotent across retries of the same dataset.
func (r *Registry) Commit(candidateZip string, opts CommitOptions) (VersionPair, error) {
	sv, err := DescribeFile(candidateZip)
	if err != nil {
		return VersionPair{}, err
	}

	if existing, ok := r.FindByKey(sv.Key); ok {
		return existing, nil
	}

	dir := pairDir(sv)

	// Guard against sanitized-name collisions with retained pairs: two
	// distinct keys must never share a per-pair directory, otherwise staging
	// would overwrite another pair's files.
	for _, old := range r.Pairs {
		if oldDir := pairDir(old.Static); oldDir == dir && old.Static.Key != sv.Key {
			return VersionPair{}, fmt.Errorf(
				"static key %q sanitizes to %s, already used by key %q",
				sv.Key, dir, old.Static.Key)
		}
	}

	pair := VersionPair{Static: sv}
	pair.Static.Path = filepath.ToSlash(filepath.Join(dir, "static.zip"))
	pair.Static.PublishedAt = time.Now().UTC()

	// 1. Stage the static ZIP into the pair directory.
	if err := r.stageFile(pair.Static.Path, candidateZip); err != nil {
		return VersionPair{}, fmt.Errorf("staging static zip: %w", err)
	}

	// 2. Stage the protos that were successfully built from this ZIP.
	cleanup := func() { _ = os.RemoveAll(r.AbsPath(dir)) }
	if opts.AlertsProto != "" {
		rel := filepath.ToSlash(filepath.Join(dir, "alerts.pb"))
		if err := r.stageFile(rel, opts.AlertsProto); err != nil {
			cleanup()
			return VersionPair{}, fmt.Errorf("staging alerts proto: %w", err)
		}
		pair.Alerts = rel
	}
	if opts.PositionsProto != "" {
		rel := filepath.ToSlash(filepath.Join(dir, "positions.pb"))
		if err := r.stageFile(rel, opts.PositionsProto); err != nil {
			cleanup()
			return VersionPair{}, fmt.Errorf("staging positions proto: %w", err)
		}
		pair.Positions = rel
	}

	// 3. Rewrite the registry, keeping at most maxPairs newest pairs.
	newPairs := make([]VersionPair, 0, maxPairs)
	newPairs = append(newPairs, pair)
	for _, old := range r.Pairs {
		if len(newPairs) >= maxPairs {
			break
		}
		newPairs = append(newPairs, old)
	}

	if err := r.writePairs(newPairs); err != nil {
		cleanup()
		return VersionPair{}, fmt.Errorf("updating version registry: %w", err)
	}

	// 4. Remove files of pairs evicted from the registry.
	retained := map[string]bool{}
	for _, p := range newPairs {
		retained[pairDir(p.Static)] = true
	}
	for _, old := range r.Pairs {
		d := pairDir(old.Static)
		if !retained[d] {
			_ = os.RemoveAll(r.AbsPath(d))
		}
	}

	r.Pairs = newPairs
	return pair, nil
}

// writePairs atomically replaces versions.json.
func (r *Registry) writePairs(pairs []VersionPair) error {
	if err := os.MkdirAll(r.root, 0o777); err != nil {
		return err
	}
	b, err := marshalRegistry(pairs)
	if err != nil {
		return err
	}
	target := registryPath(r.root)
	tmp, err := os.CreateTemp(r.root, ".versions-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
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

// PairDir returns the store-relative per-pair directory for a static key.
func PairDir(key string) string {
	return pairDir(StaticVersion{Key: key})
}
