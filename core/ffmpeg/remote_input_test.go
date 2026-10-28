package ffmpeg

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var modTime = time.Unix(1700000000, 0)

const presigned = "http://garage.garage.svc.cluster.local:3900/music/a/b.flac" +
	"?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=GKSECRETKEYID%2F20261028%2Fgarage%2Fs3%2Faws4_request" +
	"&X-Amz-Expires=300&X-Amz-Signature=deadbeef"

var _ = Describe("remote (presigned) inputs", func() {
	Describe("RedactURLs", func() {
		It("cuts the query string (where the credential lives) and keeps the object path", func() {
			got := RedactURLs("ffmpeg: " + presigned + ": Server returned 403 Forbidden")
			Expect(got).ToNot(ContainSubstring("X-Amz"))
			Expect(got).ToNot(ContainSubstring("GKSECRETKEYID"))
			Expect(got).To(ContainSubstring("http://garage.garage.svc.cluster.local:3900/music/a/b.flac?<redacted>"))
			Expect(got).To(HaveSuffix(": Server returned 403 Forbidden"))
		})

		It("leaves local paths and query-less URLs alone", func() {
			Expect(RedactURLs("/music/a/b.flac: No such file")).To(Equal("/music/a/b.flac: No such file"))
			Expect(RedactURLs("http://host/a.mp3 failed")).To(Equal("http://host/a.mp3 failed"))
		})
	})

	Describe("ProbeError", func() {
		It("never prints a presigned URL's credential, from Path or from ffprobe's stderr", func() {
			e := &ProbeError{Path: presigned, Reason: "x",
				err: &exec.ExitError{Stderr: []byte(presigned + ": Invalid data found when processing input")}}
			Expect(e.Error()).ToNot(ContainSubstring("X-Amz-Credential"))
			Expect(e.Error()).To(ContainSubstring("Invalid data found"))
			Expect((&ProbeError{Path: presigned, Reason: "file not found"}).Error()).ToNot(ContainSubstring("X-Amz"))
		})

		It("keeps the presigned URL out of the client-facing reason", func() {
			other := strings.Replace(presigned, "deadbeef", "cafef00d", 1) // e.g. a redirect target
			r := probeClientReason(&exec.ExitError{Stderr: []byte(presigned + " -> " + other + ": 403")}, presigned)
			Expect(r).ToNot(ContainSubstring("X-Amz"))
		})
	})

	Describe("guardRemoteInputs", func() {
		guard := []string{"-protocol_whitelist", remoteProtocolWhitelist, "-format_whitelist", remoteFormatWhitelist}

		It("puts the whitelists in front of ffmpeg's -i for a remote input", func() {
			args := guardRemoteInputs([]string{"ffmpeg", "-ss", "5", "-i", presigned, "-f", "mp3", "-"})
			Expect(args).To(Equal(append(append([]string{"ffmpeg", "-ss", "5"}, guard...), "-i", presigned, "-f", "mp3", "-")))
		})

		It("puts them in front of ffprobe's positional input", func() {
			args := guardRemoteInputs([]string{"ffprobe", "-v", "error", "-show_format", presigned})
			Expect(args).To(Equal(append(append([]string{"ffprobe", "-v", "error", "-show_format"}, guard...), presigned)))
		})

		It("leaves local inputs untouched", func() {
			in := []string{"ffmpeg", "-i", "/music/a.flac", "-f", "mp3", "-"}
			Expect(guardRemoteInputs(in)).To(Equal(in))
		})

		It("never allows file: or playlist demuxers", func() {
			Expect(strings.Split(remoteProtocolWhitelist, ",")).ToNot(ContainElement("file"))
			for _, f := range []string{"hls", "concat", "ffconcat", "image2", "m3u8"} {
				Expect(strings.Split(remoteFormatWhitelist, ",")).ToNot(ContainElement(f))
			}
		})
	})

	// End to end against the real binary: an object that is really an HLS playlist must not
	// make ffmpeg fetch the URL inside it (SSRF), while a real audio object still plays.
	Context("with a real ffmpeg", func() {
		var (
			ff      FFmpeg
			srv     *httptest.Server
			nested  atomic.Int32
			mp3Data []byte
		)

		BeforeEach(func() {
			ffOnce = sync.Once{}
			ff = New()
			if !ff.IsAvailable() {
				Skip("FFmpeg not available on this system")
			}
			var err error
			mp3Data, err = os.ReadFile("tests/fixtures/test.mp3")
			Expect(err).ToNot(HaveOccurred())
			nested.Store(0)
			mux := http.NewServeMux()
			mux.HandleFunc("/bucket/song.mp3", func(w http.ResponseWriter, r *http.Request) {
				http.ServeContent(w, r, "song.mp3", modTime, bytes.NewReader(mp3Data))
			})
			mux.HandleFunc("/internal/secret.mp3", func(w http.ResponseWriter, r *http.Request) {
				nested.Add(1)
				http.ServeContent(w, r, "secret.mp3", modTime, bytes.NewReader(mp3Data))
			})
			srv = httptest.NewServer(mux)
			// S3 serves the Content-Type the uploader chose. An HLS type makes ffmpeg (even
			// 7.x, which otherwise refuses HLS behind a non-standard extension) open it as a
			// playlist, which is the attack.
			mux.HandleFunc("/bucket/evil.m3u8", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\n"+srv.URL+"/internal/secret.mp3\n#EXT-X-ENDLIST\n")
			})
			DeferCleanup(srv.Close)
		})

		It("refuses a playlist object and never fetches the URL inside it", func() {
			out, err := ff.Transcode(GinkgoT().Context(), TranscodeOptions{
				Command: "ffmpeg -i %s -map 0:a:0 -c:a libmp3lame -b:a 64k -f mp3 -",
				Format:  "mp3", InputURL: srv.URL + "/bucket/evil.m3u8?X-Amz-Signature=s3cr3t",
			})
			if err == nil {
				_, err = io.ReadAll(out)
				_ = out.Close()
			}
			Expect(err).To(HaveOccurred())
			Expect(nested.Load()).To(BeZero(), "ffmpeg followed a URL from inside the object")
			Expect(err.Error()).ToNot(ContainSubstring("s3cr3t"))
		})

		It("still transcodes a real audio object over HTTP", func() {
			out, err := ff.Transcode(GinkgoT().Context(), TranscodeOptions{
				Command: "ffmpeg -i %s -map 0:a:0 -c:a libmp3lame -b:a 64k -f mp3 -",
				Format:  "mp3", InputURL: srv.URL + "/bucket/song.mp3?X-Amz-Signature=s3cr3t",
			})
			Expect(err).ToNot(HaveOccurred())
			data, err := io.ReadAll(out)
			_ = out.Close()
			Expect(err).ToNot(HaveOccurred())
			Expect(len(data)).To(BeNumerically(">", 0))
		})

		It("probes a real audio object over HTTP and refuses a playlist one", func() {
			res, err := ff.ProbeAudioStream(GinkgoT().Context(), srv.URL+"/bucket/song.mp3?X-Amz-Signature=s3cr3t")
			Expect(err).ToNot(HaveOccurred())
			Expect(res.Codec).To(Equal("mp3"))

			_, err = ff.ProbeAudioStream(GinkgoT().Context(), srv.URL+"/bucket/evil.m3u8?X-Amz-Signature=s3cr3t")
			Expect(err).To(HaveOccurred())
			var pe *ProbeError
			Expect(errors.As(err, &pe)).To(BeTrue())
			Expect(err.Error()).ToNot(ContainSubstring("s3cr3t"))
			Expect(nested.Load()).To(BeZero())
		})
	})
})
