package stream

import (
	"testing"

	_ "github.com/navidrome/navidrome/adapters/gotaglib"  // the local storage needs its default extractor
	_ "github.com/navidrome/navidrome/core/storage/local" // raw streams open through storage.For(lib)
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
