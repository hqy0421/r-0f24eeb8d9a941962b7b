package alerts

import (
	"archive/zip"
	"bytes"
	"io"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/MKuranowski/WarsawGTFS/realtime/util"
)

// fakeAlertsResource is a util.Resource returning canned bytes or an error.
type fakeAlertsResource struct {
	content  []byte
	fetchErr error
}

func (f *fakeAlertsResource) Check() (bool, error) { return true, nil }
func (f *fakeAlertsResource) LastCheck() time.Time { return time.Now() }
func (f *fakeAlertsResource) Fetch() (io.ReadCloser, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return io.NopCloser(bytes.NewReader(f.content)), nil
}

var _ util.Resource = (*fakeAlertsResource)(nil)

func alertsZip(feedVersion string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"feed_info.txt": "feed_publisher_name,feed_version\nWTP," + feedVersion + "\n",
		"routes.txt":    "route_id,route_type\n1,3\n2,0\n",
	}
	for name, content := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(content))
	}
	_ = zw.Close()
	return buf.Bytes()
}

// sameRouteMap reports whether two maps share the same backing object.
func sameRouteMap(a, b map[string]sort.StringSlice) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

func TestRoutesResourceSwapAndFailureRollback(t *testing.T) {
	res := &fakeAlertsResource{content: alertsZip("old-key")}
	rr := routesResource{Resource: res}

	if err := rr.Update(); err != nil {
		t.Fatalf("initial Update: %v", err)
	}
	if rr.StaticKey != "old-key" {
		t.Fatalf("initial key = %q", rr.StaticKey)
	}
	oldMap := rr.RouteMap

	// Fetch failure: old key and route map stay in place.
	res.fetchErr = io.ErrClosedPipe
	if err := rr.Update(); err == nil {
		t.Fatal("expected failure")
	}
	if rr.StaticKey != "old-key" || !sameRouteMap(rr.RouteMap, oldMap) {
		t.Fatal("state mutated after failed fetch")
	}

	// Corrupt archive: same guarantee.
	res.fetchErr = nil
	res.content = []byte("not a zip")
	if err := rr.Update(); err == nil {
		t.Fatal("expected failure on corrupt archive")
	}
	if rr.StaticKey != "old-key" || !sameRouteMap(rr.RouteMap, oldMap) {
		t.Fatal("state mutated after corrupt-archive failure")
	}

	// Valid new dataset: atomic swap.
	res.content = alertsZip("new-key")
	if err := rr.Update(); err != nil {
		t.Fatalf("successful Update: %v", err)
	}
	if rr.StaticKey != "new-key" {
		t.Fatalf("key = %q, want new-key", rr.StaticKey)
	}
	if sameRouteMap(rr.RouteMap, oldMap) {
		t.Fatal("route map not replaced on successful swap")
	}
}
