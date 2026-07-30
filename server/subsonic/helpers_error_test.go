package subsonic

import (
	"github.com/navidrome/navidrome/server/subsonic/responses"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("subError.Error", func() {
	It("returns a single pre-formatted message verbatim (no spurious Sprintf)", func() {
		// Regression: a lone message containing '%' (e.g. a URL-encoded S3 object key
		// in a stream.view failure) must not be re-run through Sprintf and mangled
		// into %!x(MISSING).
		msg := `Internal Server Error: blob (key "music/William%20Walton%20-%20Henry%20V.flac") (code=Unknown): read tcp`
		err := newError(responses.ErrorGeneric, msg)
		Expect(err.Error()).To(Equal(msg))
	})

	It("still applies format args when they are provided", func() {
		err := newError(responses.ErrorGeneric, "not found: %s (id %d)", "song", 42)
		Expect(err.Error()).To(Equal("not found: song (id 42)"))
	})

	It("falls back to the code's default message when none is given", func() {
		err := newError(responses.ErrorGeneric)
		Expect(err.Error()).To(Equal(responses.ErrorMsg(responses.ErrorGeneric)))
	})
})
