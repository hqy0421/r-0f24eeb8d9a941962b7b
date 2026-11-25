package gtfs

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/MKuranowski/WarsawGTFS/realtime/statickey"
)

func writeGtfsZip(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "gtfs.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o666); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStaticKeyDeclaredVersion(t *testing.T) {
	p := writeGtfsZip(t, map[string]string{
		"feed_info.txt": "feed_publisher_name,feed_version\nWTP,2026-11-25 03:00:00\n",
	})
	g, err := NewGtfsFromFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	key, err := g.StaticKey()
	if err != nil {
		t.Fatal(err)
	}
	if key != "2026-11-25 03:00:00" {
		t.Fatalf("got %q", key)
	}
}

func TestStaticKeyHashFallback(t *testing.T) {
	var zipBytes []byte
	{
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("routes.txt")
		_, _ = w.Write([]byte("route_id\n1\n"))
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		zipBytes = buf.Bytes()
	}
	p := filepath.Join(t.TempDir(), "gtfs.zip")
	if err := os.WriteFile(p, zipBytes, 0o666); err != nil {
		t.Fatal(err)
	}

	g, err := NewGtfsFromFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	key, err := g.StaticKey()
	if err != nil {
		t.Fatal(err)
	}
	want := statickey.HashPrefix + statickey.HashBytes(zipBytes)
	if key != want {
		t.Fatalf("got %q, want %q", key, want)
	}

	// Hashing must not corrupt later ZIP access: routes.txt must still open.
	if f := g.GetZipFileByName("routes.txt"); f == nil {
		t.Fatal("routes.txt unreadable after StaticKey hash fallback")
	}
}
