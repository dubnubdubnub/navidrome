package stream

import (
	"testing"

	// NewStream opens raw streams through the library storage backend (s3 fork), so the
	// local "file" scheme must be registered, as main does via the scanner import.
	_ "github.com/navidrome/navidrome/adapters/gotaglib" // the local backend's default extractor
	_ "github.com/navidrome/navidrome/core/storage/local"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/tests"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestStream(t *testing.T) {
	tests.Init(t, false)
	log.SetLevel(log.LevelFatal)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Stream Suite")
}
