package positions

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/MKuranowski/WarsawGTFS/realtime/gtfs"
	"github.com/MKuranowski/WarsawGTFS/realtime/rtstatic"
	"github.com/MKuranowski/WarsawGTFS/realtime/util"
)

// fakeResource is a util.Resource whose Fetch() returns canned bytes or an
// error, and whose Check() always reports "changed".
type fakeResource struct {
	content  []byte
	fetchErr error
}

func (f *fakeResource) Check() (bool, error) { return true, nil }
func (f *fakeResource) LastCheck() time.Time { return time.Now() }
func (f *fakeResource) Fetch() (io.ReadCloser, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return io.NopCloser(bytes.NewReader(f.content)), nil
}

var _ util.Resource = (*fakeResource)(nil)

// buildGtfsZip builds a minimal but LoadAll-compatible GTFS ZIP carrying the
// given feed_version. stop_times are intentionally empty, so no brigades are
// produced - these tests only care about key/map swap behavior.
func buildGtfsZip(t *testing.T, feedVersion string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"feed_info.txt":      "feed_publisher_name,feed_version\nWTP," + feedVersion + "\n",
		"routes.txt":         "route_id,route_type\n1,3\n",
		"stops.txt":          "stop_id,stop_lat,stop_lon\nST1,52.2,21.0\n",
		"trips.txt":          "trip_id,route_id,service_id,block_short_name\nT1,1,S1,B1\n",
		"calendar_dates.txt": "service_id,date,exception_type\nS1,20260101,1\n",
		"stop_times.txt":     "trip_id,stop_id,departure_time,stop_sequence\n",
	}
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
	return buf.Bytes()
}

func loadBrigadesFromZip(t *testing.T, b []byte) *gtfs.Gtfs {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gtfs.zip")
	if err := os.WriteFile(path, b, 0o666); err != nil {
		t.Fatal(err)
	}
	g, err := gtfs.NewGtfsFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	if err := g.LoadAll(); err != nil {
		t.Fatal(err)
	}
	return g
}

// sameMap reports whether two maps share the same backing object.
func sameMap(a, b map[string][]*brigadeEntry) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

func TestBrigadesResourceSwapAndFailureRollback(t *testing.T) {
	// Seed the resource with the "old" dataset and load it.
	oldZip := buildGtfsZip(t, "old-version")
	res := &fakeResource{content: oldZip}
	br := brigadesResource{Resource: res}

	if err := br.Update(); err != nil {
		t.Fatalf("initial Update: %v", err)
	}
	if br.StaticKey != "old-version" {
		t.Fatalf("initial key = %q, want old-version", br.StaticKey)
	}
	if br.BrigadeMap == nil {
		t.Fatal("brigade map not initialized")
	}
	oldMap := br.BrigadeMap

	// Simulate a fetch failure for the "new" dataset: the old key and map
	// must remain in place.
	res.fetchErr = io.ErrClosedPipe
	if err := br.Update(); err == nil {
		t.Fatal("expected Update to fail when Fetch fails")
	}
	if br.StaticKey != "old-version" {
		t.Fatalf("key changed after failed fetch: %q", br.StaticKey)
	}
	if !sameMap(br.BrigadeMap, oldMap) {
		t.Fatal("brigade map replaced after failed fetch")
	}

	// Simulate a corrupt new archive: same guarantee.
	res.fetchErr = nil
	res.content = []byte("not a zip")
	if err := br.Update(); err == nil {
		t.Fatal("expected Update to fail on a corrupt archive")
	}
	if br.StaticKey != "old-version" || !sameMap(br.BrigadeMap, oldMap) {
		t.Fatal("state mutated after a corrupt-archive failure")
	}

	// A valid new dataset swaps key and map together, atomically.
	res.content = buildGtfsZip(t, "new-version")
	if err := br.Update(); err != nil {
		t.Fatalf("successful Update: %v", err)
	}
	if br.StaticKey != "new-version" {
		t.Fatalf("key = %q, want new-version", br.StaticKey)
	}
	if sameMap(br.BrigadeMap, oldMap) {
		t.Fatal("brigade map was not replaced on a successful swap")
	}
}

func TestCreateStampsCurrentStaticKey(t *testing.T) {
	// End-to-end Create() run with a stub API: the written proto header must
	// carry the static key from Options, verifiable with rtstatic.
	zipBytes := buildGtfsZip(t, "stamp-test")
	g := loadBrigadesFromZip(t, zipBytes)
	brigadeMap, err := loadBrigades(g)
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "positions.pb")
	opts := Options{GtfsRtTarget: out, StaticKey: "stamp-test"}

	// No active vehicles: an empty entity set still produces a signed header.
	// buildWarsawRequest hardcodes the real API host, so redirect that host to
	// a local server via the test client's transport.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"result":[]}`)
	}))
	defer server.Close()

	client := server.Client()
	prevTransport := client.Transport
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = server.Listener.Addr().String()
		req.Host = ""
		return prevTransport.RoundTrip(req)
	})
	api := VehicleAPI{Key: "test-key", Client: client}

	_, err = Create(api, brigadeMap, nil, opts)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := rtstatic.Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	key, err := rtstatic.StaticKey(msg)
	if err != nil {
		t.Fatal(err)
	}
	if key != "stamp-test" {
		t.Fatalf("proto key = %q, want stamp-test", key)
	}
}

// roundTripFunc adapts a function into an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
