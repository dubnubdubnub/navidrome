package ffmpeg

import (
	"errors"
	"os/exec"
	"slices"
	"strings"

	"github.com/navidrome/navidrome/resources"
	"gopkg.in/yaml.v3"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	presignedFLAC = "http://garage.garage.svc.cluster.local:3900/music/Artist/Album/01%20Track.flac" +
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=GK123%2F20261028%2Fgarage%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20261028T100000Z&X-Amz-Expires=3600&X-Amz-SignedHeaders=host&X-Amz-Signature=deadbeef"
	presignedM4A = "https://s3.example/music/a/b.m4a?X-Amz-Signature=cafe&X-Amz-Security-Token=tok"
)

// inputIndex returns the position of input in args.
func inputIndex(args []string, input string) int {
	return slices.Index(args, input)
}

var _ = Describe("remote ffmpeg inputs", func() {
	BeforeEach(func() {
		_, _ = ffmpegCmd()
		ffmpegPath = "ffmpeg"
		ffmpegErr = nil
	})

	Describe("guardRemoteInput", func() {
		It("leaves a local input untouched", func() {
			args := []string{"ffmpeg", "-i", "/music/a.flac", "-f", "mp3", "-"}
			out, err := guardRemoteInput(args, "/music/a.flac")
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(Equal(args))
		})

		It("pins the demuxer and protocols right before the URL's -i (http)", func() {
			args := []string{"ffmpeg", "-ss", "30", "-i", presignedFLAC, "-map", "0:a:0", "-f", "mp3", "-"}
			out, err := guardRemoteInput(args, presignedFLAC)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(Equal([]string{"ffmpeg", "-ss", "30",
				"-protocol_whitelist", "http,tcp", "-f", "flac",
				"-i", presignedFLAC, "-map", "0:a:0", "-f", "mp3", "-"}))
			Expect(args[3]).To(Equal("-i"), "the caller's slice must not be modified")
		})

		It("allows only https+tls+tcp for an https URL and maps m4a to the mov demuxer", func() {
			out, err := guardRemoteInput([]string{"ffmpeg", "-i", presignedM4A, "-"}, presignedM4A)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(Equal([]string{"ffmpeg", "-protocol_whitelist", "https,tls,tcp", "-f", "mov", "-i", presignedM4A, "-"}))
		})

		It("puts the options before a bare input (ffprobe style)", func() {
			out, err := guardRemoteInput([]string{"ffprobe", "-v", "error", presignedFLAC}, presignedFLAC)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(Equal([]string{"ffprobe", "-v", "error", "-protocol_whitelist", "http,tcp", "-f", "flac", presignedFLAC}))
		})

		It("derives the demuxer from the URL path, not the query, case-insensitively", func() {
			u := "http://h/music/x.FLAC?X-Amz-Signature=a.m3u8"
			out, err := guardRemoteInput([]string{"ffmpeg", "-i", u}, u)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(ContainElements("-f", "flac"))
		})

		DescribeTable("refuses what it cannot pin",
			func(args []string, input string) {
				_, err := guardRemoteInput(args, input)
				Expect(err).To(HaveOccurred())
			},
			Entry("a playlist suffix", []string{"ffmpeg", "-i", "http://h/x.m3u8"}, "http://h/x.m3u8"),
			Entry("a concat list", []string{"ffmpeg", "-i", "http://h/x.ffconcat"}, "http://h/x.ffconcat"),
			Entry("no suffix", []string{"ffmpeg", "-i", "http://h/x"}, "http://h/x"),
			Entry("a template embedding the input", []string{"ffmpeg", "-i", "file:" + presignedFLAC}, presignedFLAC),
			Entry("the input used twice", []string{"ffmpeg", "-i", presignedFLAC, "-i", presignedFLAC}, presignedFLAC),
		)
	})

	It("pins a demuxer for every audio suffix Navidrome scans", func() {
		f, err := resources.FS().Open("mime_types.yaml")
		Expect(err).ToNot(HaveOccurred())
		defer f.Close()
		var mt struct {
			Types map[string]string `yaml:"types"`
		}
		Expect(yaml.NewDecoder(f).Decode(&mt)).To(Succeed())
		playlists := []string{"audio/mpegurl", "audio/x-mpegurl", "audio/x-scpls"} // model.IsAudioFile excludes these
		var missing []string
		for ext, typ := range mt.Types {
			if strings.HasPrefix(typ, "audio/") && !slices.Contains(playlists, typ) {
				if _, ok := remoteDemuxers[strings.TrimPrefix(ext, ".")]; !ok {
					missing = append(missing, ext)
				}
			}
		}
		Expect(missing).To(BeEmpty(), "add these to remoteDemuxers, or s3 playback of them is refused")
		for _, d := range remoteDemuxers {
			Expect(d).ToNot(BeElementOf("hls", "concat", "applehttp"))
		}
	})

	Describe("command builders", func() {
		expectGuarded := func(args []string, input, demuxer string) {
			i := inputIndex(args, input)
			Expect(i).To(BeNumerically(">=", 5))
			Expect(args[i-5 : i]).To(Equal([]string{"-protocol_whitelist", "http,tcp", "-f", demuxer, "-i"}))
		}

		It("guards a default (dynamic) transcode", func() {
			args, err := transcodeArgs(TranscodeOptions{Command: defaultCommands["mp3"], Format: "mp3",
				FilePath: "/music/x.flac", InputURL: presignedFLAC, BitRate: 128, Offset: 10})
			Expect(err).ToNot(HaveOccurred())
			expectGuarded(args, presignedFLAC, "flac")
			Expect(args[1:3]).To(Equal([]string{"-ss", "10"}))
		})

		It("guards a custom template transcode", func() {
			args, err := transcodeArgs(TranscodeOptions{Command: "ffmpeg -i %s -b:a %bk -f mp3 -", Format: "mp3",
				FilePath: "/music/x.flac", InputURL: presignedFLAC, BitRate: 192})
			Expect(err).ToNot(HaveOccurred())
			expectGuarded(args, presignedFLAC, "flac")
		})

		It("refuses a custom template it cannot guard rather than running it unguarded", func() {
			_, err := transcodeArgs(TranscodeOptions{Command: "ffmpeg -i async:%s -f mp3 -", Format: "mp3",
				FilePath: "/music/x.flac", InputURL: presignedFLAC})
			Expect(err).To(HaveOccurred())
		})

		It("leaves a local transcode alone", func() {
			args, err := transcodeArgs(TranscodeOptions{Command: defaultCommands["mp3"], Format: "mp3", FilePath: "/music/x.flac"})
			Expect(err).ToNot(HaveOccurred())
			Expect(args).ToNot(ContainElement("-protocol_whitelist"))
		})

		It("guards embedded-art extraction", func() {
			args, err := extractImageArgs(presignedFLAC)
			Expect(err).ToNot(HaveOccurred())
			expectGuarded(args, presignedFLAC, "flac")
		})

		It("guards ffprobe (bare input)", func() {
			args, err := probeAudioStreamArgs(presignedFLAC)
			Expect(err).ToNot(HaveOccurred())
			i := inputIndex(args, presignedFLAC)
			Expect(i).To(Equal(len(args) - 1))
			Expect(args[i-4 : i]).To(Equal([]string{"-protocol_whitelist", "http,tcp", "-f", "flac"}))
		})
	})

	Describe("Redact", func() {
		It("blanks every X-Amz-* value and keeps host, bucket and key", func() {
			r := Redact(presignedFLAC)
			Expect(r).To(ContainSubstring("garage.garage.svc.cluster.local:3900/music/Artist/Album/01%20Track.flac"))
			Expect(r).ToNot(ContainSubstring("deadbeef"))
			Expect(r).ToNot(ContainSubstring("GK123"))
			Expect(r).To(ContainSubstring("X-Amz-Signature=REDACTED"))
			Expect(r).To(ContainSubstring("X-Amz-Credential=REDACTED"))
		})

		It("redacts a URL embedded in free text, such as ffmpeg's stderr", func() {
			msg := "[http @ 0x1] HTTP error 403 Forbidden\n" + presignedM4A + ": Server returned 403 Forbidden"
			r := Redact(msg)
			Expect(r).ToNot(ContainSubstring("cafe"))
			Expect(r).ToNot(ContainSubstring("tok"))
			Expect(r).To(ContainSubstring("Server returned 403 Forbidden"))
		})

		It("leaves a string without credentials as is", func() {
			Expect(Redact("/music/a.flac")).To(Equal("/music/a.flac"))
		})

		It("is applied to ProbeError.Error(), including ffprobe's stderr", func() {
			e := &ProbeError{Path: Redact(presignedFLAC), Reason: "could not read file",
				err: &exec.ExitError{Stderr: []byte(presignedFLAC + ": Server returned 403 Forbidden")}}
			Expect(e.Error()).ToNot(ContainSubstring("deadbeef"))
			Expect(e.Error()).To(ContainSubstring("Server returned 403 Forbidden"))

			e = &ProbeError{Path: presignedFLAC, Reason: "x", err: errors.New("open " + presignedFLAC)}
			Expect(e.Error()).ToNot(ContainSubstring("deadbeef"))
		})

		It("is applied to the ffmpeg exit error built from stderr", func() {
			Expect(Redact("ffmpeg exited with non-zero status code: 1: " + presignedFLAC)).ToNot(ContainSubstring("deadbeef"))
		})
	})
})
