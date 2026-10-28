package s3

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/navidrome/navidrome/adapters/gotaglib" // the default tag extractor
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core/storage"
)

// fakeS3 is just enough of an S3 endpoint for bucket-level calls: HEAD /<bucket>/ answers
// 200 for the buckets it holds and 404 otherwise (path-style addressing).
func fakeS3(t *testing.T, buckets ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var heads atomic.Int32
	have := map[string]bool{}
	for _, b := range buckets {
		have[b] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := strings.Trim(r.URL.Path, "/")
		if r.Method == http.MethodHead && !strings.Contains(b, "/") {
			heads.Add(1)
			if have[b] {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &heads
}

// useEndpoint points the backend at srv for the duration of the test and resets the
// package-level client cache before and after.
func useEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	prev := conf.Server.S3
	conf.Server.S3.Endpoint = endpoint
	conf.Server.S3.Region = "garage" // a fixed region: no GetBucketLocation round trip
	conf.Server.S3.PathStyle = true
	conf.Server.S3.AccessKey = "GKtest"
	conf.Server.S3.SecretKey = "secret"
	resetCache := func() {
		fsCacheMu.Lock()
		fsCache = map[fsKey]*s3FS{}
		fsCacheMu.Unlock()
	}
	resetCache()
	t.Cleanup(func() {
		conf.Server.S3 = prev
		resetCache()
	})
}

func musicFS(t *testing.T, uri string) (storage.MusicFS, error) {
	t.Helper()
	st, err := storage.For(uri)
	if err != nil {
		t.Fatalf("storage.For(%q): %v", uri, err)
	}
	return st.FS()
}

func TestFSReusesOneClientPerBucket(t *testing.T) {
	srv, _ := fakeS3(t, "music")
	useEndpoint(t, srv.URL)

	a, err := musicFS(t, "s3://music")
	if err != nil {
		t.Fatal(err)
	}
	b, err := musicFS(t, "s3://music")
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a.(*s3FS).client != b.(*s3FS).client {
		t.Fatal("two storage.For(s3://music) calls built two clients; every request would leak a transport")
	}

	other, err := musicFS(t, "s3://music-24bit-originals")
	if err != nil {
		t.Fatal(err)
	}
	if other == a {
		t.Fatal("different buckets must not share an s3FS")
	}

	conf.Server.S3.SecretKey = "rotated"
	rotated, err := musicFS(t, "s3://music")
	if err != nil {
		t.Fatal(err)
	}
	if rotated == a {
		t.Fatal("a credential change must build a new client, not reuse the stale one")
	}
}

func TestFSDoesNotCacheErrors(t *testing.T) {
	useEndpoint(t, "")
	t.Setenv("ND_S3_ENDPOINT", "")
	if _, err := musicFS(t, "s3://music"); err == nil {
		t.Fatal("expected an error with no endpoint configured")
	}
	srv, _ := fakeS3(t, "music")
	conf.Server.S3.Endpoint = srv.URL
	if _, err := musicFS(t, "s3://music"); err != nil {
		t.Fatalf("a fixed config must work on the next call, got %v", err)
	}
}

func TestLibraryURIMustNameAWholeBucket(t *testing.T) {
	srv, _ := fakeS3(t, "music")
	useEndpoint(t, srv.URL)

	for _, ok := range []string{"s3://music", "s3://music/", "s3:///music"} {
		if _, err := musicFS(t, ok); err != nil {
			t.Errorf("%s: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"s3://music/rock", "s3://music/a/b", "s3:///music/rock", "s3://"} {
		if _, err := musicFS(t, bad); err == nil {
			t.Errorf("%s: a prefix (or no bucket) must be rejected, not widened to the whole bucket", bad)
		}
	}
}

func TestStatRootChecksTheBucketExists(t *testing.T) {
	srv, heads := fakeS3(t, "music")
	useEndpoint(t, srv.URL)

	fsys, err := musicFS(t, "s3://music")
	if err != nil {
		t.Fatal(err)
	}
	info, err := fs.Stat(fsys, ".")
	if err != nil || !info.IsDir() {
		t.Fatalf("existing bucket: info=%v err=%v", info, err)
	}
	if heads.Load() == 0 {
		t.Fatal("Stat(\".\") answered without asking the server")
	}

	missing, err := musicFS(t, "s3://no-such-bucket")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fs.Stat(missing, ".")
	if !errors.Is(err, fs.ErrNotExist) || !os.IsNotExist(err) {
		t.Fatalf("missing bucket must stat as not-exist (library validation maps that to pathNotFound), got %v", err)
	}
}

func TestStatRootFailsWhenEndpointIsDown(t *testing.T) {
	srv, _ := fakeS3(t, "music")
	useEndpoint(t, srv.URL)
	fsys, err := musicFS(t, "s3://music")
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if _, err := fs.Stat(fsys, "."); err == nil {
		t.Fatal("an unreachable endpoint must not read as an existing, empty library")
	}
}

func TestPresignedURLRequiresAnExpiry(t *testing.T) {
	srv, _ := fakeS3(t, "music")
	useEndpoint(t, srv.URL)
	fsys, err := musicFS(t, "s3://music")
	if err != nil {
		t.Fatal(err)
	}
	up := fsys.(storage.URLProvider)
	if _, err := up.PresignedURL(t.Context(), "a/b.flac", 0); err == nil {
		t.Fatal("a zero expiry must be refused")
	}
	u, err := up.PresignedURL(t.Context(), "a/b.flac", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "X-Amz-Expires=300") {
		t.Fatalf("expiry not honoured: %s", u)
	}
}
