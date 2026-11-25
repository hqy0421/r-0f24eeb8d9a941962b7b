package statickey

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// makeZip builds an in-memory GTFS-like ZIP from name→content pairs.
func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func zipReader(t *testing.T, b []byte) *zip.Reader {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	return zr
}

func writeTempZip(t *testing.T, b []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gtfs.zip")
	if err := os.WriteFile(p, b, 0o666); err != nil {
		t.Fatalf("write temp zip: %v", err)
	}
	return p
}

func TestReadFeedVersion(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{
			name:    "simple",
			content: "feed_publisher_name,feed_version\nWTP,2026-11-25 03:00:00\n",
			want:    "2026-11-25 03:00:00",
		},
		{
			name:    "first non-empty wins",
			content: "feed_publisher_name,feed_version\nWTP,\nWTP2,second-version\n",
			want:    "second-version",
		},
		{
			name:    "missing column",
			content: "feed_publisher_name,feed_lang\nWTP,pl\n",
			wantErr: true,
		},
		{
			name:    "all empty",
			content: "feed_publisher_name,feed_version\nWTP,\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadFeedVersion(bytes.NewBufferString(tc.content))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeyFromZipDeclaredVersion(t *testing.T) {
	zr := zipReader(t, makeZip(t, map[string]string{
		"feed_info.txt": "feed_publisher_name,feed_version\nWTP,2026-11-25 03:00:00\n",
		"routes.txt":    "route_id\n1\n",
	}))
	key, fv, err := KeyFromZip(zr, "deadbeef")
	if err != nil {
		t.Fatalf("KeyFromZip: %v", err)
	}
	if key != "2026-11-25 03:00:00" || fv != "2026-11-25 03:00:00" {
		t.Fatalf("unexpected key=%q fv=%q", key, fv)
	}
}

func TestKeyFromZipHashFallback(t *testing.T) {
	t.Run("no feed_info", func(t *testing.T) {
		zr := zipReader(t, makeZip(t, map[string]string{"routes.txt": "route_id\n1\n"}))
		key, fv, err := KeyFromZip(zr, "abc123")
		if err != nil {
			t.Fatalf("KeyFromZip: %v", err)
		}
		if key != HashPrefix+"abc123" || fv != "" {
			t.Fatalf("unexpected key=%q fv=%q", key, fv)
		}
	})
	t.Run("empty feed_version", func(t *testing.T) {
		zr := zipReader(t, makeZip(t, map[string]string{
			"feed_info.txt": "feed_publisher_name,feed_version\nWTP,\n",
		}))
		key, _, err := KeyFromZip(zr, "abc123")
		if err != nil {
			t.Fatalf("KeyFromZip: %v", err)
		}
		if key != HashPrefix+"abc123" {
			t.Fatalf("unexpected key=%q", key)
		}
	})
}

func TestDescribeFile(t *testing.T) {
	zipBytes := makeZip(t, map[string]string{
		"feed_info.txt": "feed_publisher_name,feed_version\nWTP,v1\n",
	})
	path := writeTempZip(t, zipBytes)

	sv, err := DescribeFile(path)
	if err != nil {
		t.Fatalf("DescribeFile: %v", err)
	}
	if sv.Key != "v1" || sv.FeedVersion != "v1" {
		t.Fatalf("unexpected static version: %+v", sv)
	}
	if sv.SHA256 != HashBytes(zipBytes) {
		t.Fatal("sha256 mismatch")
	}
}

func TestRegistryCommitKeepsTwoPairs(t *testing.T) {
	root := t.TempDir()
	reg, err := LoadRegistry(root)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}

	versions := []string{"v1", "v2", "v3"}
	for i, version := range versions {
		zipBytes := makeZip(t, map[string]string{
			"feed_info.txt": "feed_publisher_name,feed_version\nWTP," + version + "\n",
		})
		candidate := writeTempZip(t, zipBytes)
		protoPath := filepath.Join(t.TempDir(), "positions.pb")
		if err := os.WriteFile(protoPath, []byte("proto-bytes-"+version), 0o666); err != nil {
			t.Fatal(err)
		}

		pair, err := reg.Commit(candidate, CommitOptions{PositionsProto: protoPath})
		if err != nil {
			t.Fatalf("Commit %s: %v", version, err)
		}
		if pair.Static.Key != version {
			t.Fatalf("pair key = %q, want %q", pair.Static.Key, version)
		}

		// The staged files must exist inside the store.
		if _, err := os.Stat(reg.AbsPath(pair.Static.Path)); err != nil {
			t.Fatalf("staged static missing: %v", err)
		}
		if _, err := os.Stat(reg.AbsPath(pair.Positions)); err != nil {
			t.Fatalf("staged proto missing: %v", err)
		}

		if i >= 1 {
			// The previous pair must remain readable after each new commit.
			if _, ok := reg.FindByKey(versions[i-1]); !ok {
				t.Fatalf("previous pair %q not retained after committing %s", versions[i-1], version)
			}
		}
	}

	// ...but only two pairs total, newest first.
	if len(reg.Pairs) != 2 {
		t.Fatalf("got %d pairs, want 2", len(reg.Pairs))
	}
	if reg.Pairs[0].Static.Key != "v3" || reg.Pairs[1].Static.Key != "v2" {
		t.Fatalf("unexpected pair order: %q, %q", reg.Pairs[0].Static.Key, reg.Pairs[1].Static.Key)
	}
	if _, ok := reg.FindByKey("v1"); ok {
		t.Fatal("evicted pair v1 should no longer be findable")
	}

	// The evicted v1 directory must have been removed.
	if _, err := os.Stat(reg.AbsPath(PairDir("v1"))); !os.IsNotExist(err) {
		t.Fatalf("evicted pair directory still present: %v", err)
	}

	// Registry must survive a reload from disk.
	reloaded, err := LoadRegistry(root)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Pairs) != 2 || reloaded.Pairs[0].Static.Key != "v3" {
		t.Fatalf("reloaded registry mismatch: %+v", reloaded.Pairs)
	}
}

func TestRegistryCommitIdempotent(t *testing.T) {
	root := t.TempDir()
	reg, err := LoadRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes := makeZip(t, map[string]string{
		"feed_info.txt": "feed_publisher_name,feed_version\nWTP,v-same\n",
	})
	candidate := writeTempZip(t, zipBytes)

	first, err := reg.Commit(candidate, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := reg.Commit(candidate, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Static.Key != second.Static.Key || len(reg.Pairs) != 1 {
		t.Fatalf("re-committing the same dataset must not create a new pair: %+v", reg.Pairs)
	}
}

func TestRegistryCommitFailureLeavesOldPairReadable(t *testing.T) {
	root := t.TempDir()
	reg, err := LoadRegistry(root)
	if err != nil {
		t.Fatal(err)
	}

	// Seed a good "old" pair.
	oldZip := writeTempZip(t, makeZip(t, map[string]string{
		"feed_info.txt": "feed_publisher_name,feed_version\nWTP,old\n",
	}))
	oldPair, err := reg.Commit(oldZip, CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// A nonexistent candidate must fail without mutating the registry.
	if _, err := reg.Commit(filepath.Join(t.TempDir(), "missing.zip"), CommitOptions{}); err == nil {
		t.Fatal("expected error for missing candidate")
	}
	if len(reg.Pairs) != 1 || reg.Pairs[0].Static.Key != "old" {
		t.Fatalf("registry mutated after failed commit: %+v", reg.Pairs)
	}
	if _, err := os.Stat(reg.AbsPath(oldPair.Static.Path)); err != nil {
		t.Fatalf("old pair no longer readable: %v", err)
	}

	// A proto path that does not exist must fail and roll back the staged pair.
	newZip := writeTempZip(t, makeZip(t, map[string]string{
		"feed_info.txt": "feed_publisher_name,feed_version\nWTP,new\n",
	}))
	if _, err := reg.Commit(newZip, CommitOptions{
		PositionsProto: filepath.Join(t.TempDir(), "nope.pb"),
	}); err == nil {
		t.Fatal("expected error for missing proto")
	}
	if len(reg.Pairs) != 1 || reg.Pairs[0].Static.Key != "old" {
		t.Fatalf("registry mutated after proto staging failure: %+v", reg.Pairs)
	}
	if _, err := os.Stat(reg.AbsPath(PairDir("new"))); !os.IsNotExist(err) {
		t.Fatal("staged new-pair directory not rolled back")
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"2026-11-25 03:00:00": "2026-11-25_03_00_00",
		"sha256:abcdef":       "sha256_abcdef",
		"plain":               "plain",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegistryCommitRejectsSanitizedNameCollision(t *testing.T) {
	root := t.TempDir()
	reg, err := LoadRegistry(root)
	if err != nil {
		t.Fatal(err)
	}

	// "a b" and "a:b" sanitize to the same slug but are distinct keys.
	first := writeTempZip(t, makeZip(t, map[string]string{
		"feed_info.txt": "feed_publisher_name,feed_version\nWTP,a b\n",
	}))
	if _, err := reg.Commit(first, CommitOptions{}); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	second := writeTempZip(t, makeZip(t, map[string]string{
		"feed_info.txt": "feed_publisher_name,feed_version\nWTP,a:b\n",
	}))
	if _, err := reg.Commit(second, CommitOptions{}); err == nil {
		t.Fatal("expected sanitized-name collision error")
	}
	if len(reg.Pairs) != 1 || reg.Pairs[0].Static.Key != "a b" {
		t.Fatalf("registry must stay untouched after collision: %+v", reg.Pairs)
	}
}
