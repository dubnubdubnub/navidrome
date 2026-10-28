package ffmpeg

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Remote inputs are presigned S3 GET URLs for s3:// libraries (see storage.URLProvider).
// ffmpeg/ffprobe fetch them themselves, so the bytes they parse are whatever a library
// uploader put in the bucket. Left to probe the format, ffmpeg would happily run the HLS or
// concat demuxer on a text playlist renamed to .mp3, and those open further URLs, i.e. any
// host the pod can reach (in-cluster services, the cloud metadata address, ...): an SSRF.
//
// guardRemoteInput closes that with two input options placed before the URL's -i:
//
//   - -f <demuxer>, chosen from the file suffix. This is the actual fix: a forced audio
//     demuxer never interprets the bytes as a playlist, so no nested open happens at all.
//     (-allowed_extensions is an HLS-demuxer option and is moot once HLS cannot be selected.)
//   - -protocol_whitelist limited to the URL's own scheme plus its transport. libavformat
//     applies an input's whitelist to every nested open the demuxers make through io_open
//     (hls, concat, mov drefs, ...), so this keeps file:, data:, pipe:, ftp:, rtmp: and
//     friends out even if a demuxer ever does open something. It does NOT restrict hosts:
//     an http playlist could still name another http host, which is why -f is required.
//
// A suffix with no known demuxer is refused rather than probed: failing loudly is better
// than reopening the hole for an obscure format.

// remoteDemuxers maps a library file suffix (lowercase, no dot) to the ffmpeg demuxer that
// reads it. It must cover every audio suffix in resources/mime_types.yaml (a spec checks);
// extend it deliberately, never with a playlist/container-of-URLs demuxer (hls, concat).
var remoteDemuxers = map[string]string{
	"mp3":  "mp3",
	"mp2":  "mp3",
	"flac": "flac",
	"ogg":  "ogg",
	"oga":  "ogg",
	"opus": "ogg",
	"spx":  "ogg",
	"m4a":  "mov",
	"m4b":  "mov",
	"m4p":  "mov",
	"mp4":  "mov",
	"alac": "mov",
	"aac":  "aac",
	"wav":  "wav",
	"aif":  "aiff",
	"aiff": "aiff",
	"aifc": "aiff",
	"wma":  "asf",
	"ape":  "ape",
	"wv":   "wv",
	"wvp":  "wv",
	"shn":  "shn",
	"mpc":  "mpc",
	"tta":  "tta",
	"tak":  "tak",
	"dsf":  "dsf",
	"dff":  "iff",
	"mka":  "matroska",
	"webm": "matroska",
	"caf":  "caf",
	"ac3":  "ac3",
	"dts":  "dts",
}

// isRemoteInput reports whether the ffmpeg input is an http(s) URL rather than a local path.
func isRemoteInput(input string) bool {
	return strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://")
}

// remoteInputOptions returns the input options that pin a remote input's demuxer and
// protocols (see guardRemoteInput).
func remoteInputOptions(input string) ([]string, error) {
	u, err := url.Parse(input)
	if err != nil {
		return nil, fmt.Errorf("remote ffmpeg input is not a valid URL: %w", err)
	}
	var protocols string
	switch u.Scheme {
	case "http":
		protocols = "http,tcp"
	case "https":
		protocols = "https,tls,tcp"
	default:
		return nil, fmt.Errorf("remote ffmpeg input has unsupported scheme %q", u.Scheme)
	}
	suffix := strings.TrimPrefix(strings.ToLower(path.Ext(u.Path)), ".")
	demuxer, ok := remoteDemuxers[suffix]
	if !ok {
		return nil, fmt.Errorf("refusing remote ffmpeg input with suffix %q: no pinned demuxer for it", suffix)
	}
	return []string{"-protocol_whitelist", protocols, "-f", demuxer}, nil
}

// guardRemoteInput inserts remoteInputOptions right before input's "-i" (or before input
// itself when the command passes it bare, as ffprobe's does). Local inputs are returned
// unchanged. The input must appear exactly once as a whole argument; a custom transcoding
// template that embeds it differently (e.g. "file:%s") cannot be guarded and is refused.
func guardRemoteInput(args []string, input string) ([]string, error) {
	if !isRemoteInput(input) {
		return args, nil
	}
	opts, err := remoteInputOptions(input)
	if err != nil {
		return nil, err
	}
	idx := -1
	for i, a := range args {
		if a == input {
			if idx >= 0 {
				return nil, fmt.Errorf("refusing remote ffmpeg input: command uses it more than once")
			}
			idx = i
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("refusing remote ffmpeg input: command does not take it as a plain argument")
	}
	at := idx
	if idx > 0 && args[idx-1] == "-i" {
		at = idx - 1
	}
	return slices.Insert(slices.Clone(args), at, opts...), nil
}

// amzQueryParam matches an AWS SigV4 presign query parameter (X-Amz-Signature,
// X-Amz-Credential, X-Amz-Security-Token, ...) and its value.
var amzQueryParam = regexp.MustCompile(`(?i)\b(X-Amz-[A-Za-z0-9-]+)=[^&\s"'<>]*`)

// Redact blanks the values of X-Amz-* query parameters anywhere in s, so a presigned URL
// in an error, ffmpeg's stderr or a log line no longer grants access. Everything else
// (host, bucket, key) is kept for debugging.
func Redact(s string) string {
	if !strings.Contains(strings.ToLower(s), "x-amz-") {
		return s
	}
	return amzQueryParam.ReplaceAllString(s, "$1=REDACTED")
}

// redactArgs returns a copy of args safe to log.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = Redact(a)
	}
	return out
}
