package s3_test

import (
	"net/url"
	"strings"
	"testing"

	_ "github.com/navidrome/navidrome/adapters/gotaglib"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core/storage"
	_ "github.com/navidrome/navidrome/core/storage/s3"
)

// TestRegistration verifies the s3 backend registers itself under the "s3" scheme and
// that storage.For resolves it. This runs without any network access.
func TestRegistration(t *testing.T) {
	st, err := storage.For("s3://music")
	if err != nil {
		t.Fatalf("storage.For(s3://music): %v", err)
	}
	if st == nil {
		t.Fatal("storage.For returned nil Storage")
	}
}

// TestFSRequiresEndpoint verifies that FS() fails clearly when no endpoint is configured,
// rather than silently succeeding (per repo convention: prefer errors over silent failure).
func TestFSRequiresEndpoint(t *testing.T) {
	// Ensure no endpoint configured for this case.
	prev := conf.Server.S3.Endpoint
	conf.Server.S3.Endpoint = ""
	t.Setenv("ND_S3_ENDPOINT", "")
	defer func() { conf.Server.S3.Endpoint = prev }()

	u, _ := url.Parse("s3://music")
	_ = u
	st, err := storage.For("s3://music")
	if err != nil {
		t.Fatalf("storage.For: %v", err)
	}
	_, err = st.FS()
	if err == nil {
		t.Fatal("expected FS() to fail with no endpoint configured")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("expected endpoint error, got: %v", err)
	}
}
