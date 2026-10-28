package artwork

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/navidrome/navidrome/core/storage"
	"github.com/navidrome/navidrome/model"
)

// libraryView bundles the MusicFS for a library with its absolute root path,
// so readers can open library-relative paths through FS and compose absolute
// paths (for ffmpeg, which is path-based) via Abs.
type libraryView struct {
	FS      storage.MusicFS
	absRoot string
}

// Abs returns the absolute path for a library-relative path. Returns "" for an
// empty rel so callers (fromFFmpegTag) can treat it as "no path available".
func (v libraryView) Abs(rel string) string {
	if rel == "" {
		return ""
	}
	return filepath.Join(v.absRoot, rel)
}

// artPresignExpiry bounds the presigned URL ffmpeg uses to pull embedded art out of an s3
// object. Extraction reads the head of one file and takes seconds; the URL is a bearer
// credential, so it should not outlive that by much.
const artPresignExpiry = 5 * time.Minute

// ffmpegInput returns the input ffmpeg should read for the given library-relative path:
// a presigned HTTP URL when the library's backend supports it (s3://), otherwise the
// absolute on-disk path (file://). Returns "" for an empty rel so callers can treat it
// as "no input available" and fall through.
func (v libraryView) ffmpegInput(ctx context.Context, rel string) string {
	if rel == "" {
		return ""
	}
	if up, ok := v.FS.(storage.URLProvider); ok {
		if u, err := up.PresignedURL(ctx, rel, artPresignExpiry); err == nil {
			return u
		}
		// Presigning failed; nothing readable for a remote backend (no local path
		// exists), so signal "no input" rather than handing ffmpeg a bogus path.
		return ""
	}
	return v.Abs(rel)
}

// isRemote reports whether the library's files have no OS path that could be re-opened
// later with os.Open/os.Stat. That is a backend reached over HTTP, i.e. one implementing
// storage.URLProvider (s3://), the same test ffmpegInput uses to hand ffmpeg a URL.
func (v libraryView) isRemote() bool {
	_, ok := v.FS.(storage.URLProvider)
	return ok
}

// withBacking keeps a resolution's sourcePath/refMtime only for a local library. For a
// remote one it clears both, so placeBytes copies the bytes into the image store and
// openOriginal serves them from there, instead of os.Open-ing a path that does not exist
// on disk (filepath.Join even mangles "s3://bucket" into "s3:/bucket").
func (v libraryView) withBacking(res resolution) resolution {
	if v.isRemote() {
		res.sourcePath, res.refMtime = "", 0
	}
	return res
}

// loadLibraryView resolves the MusicFS and absolute root path in a single
// library lookup.
func loadLibraryView(ctx context.Context, ds model.DataStore, libID int) (libraryView, error) {
	lib, err := ds.Library(ctx).Get(libID)
	if err != nil {
		return libraryView{}, err
	}
	s, err := storage.For(lib.Path)
	if err != nil {
		return libraryView{}, err
	}
	fs, err := s.FS()
	if err != nil {
		return libraryView{}, err
	}
	return libraryView{FS: fs, absRoot: localOSRoot(lib.Path)}, nil
}

// localOSRoot maps a library path to its on-disk root so Abs() yields paths os.Open/os.Stat accept:
// a file:// URL becomes its parsed OS path (bare paths already are; non-local schemes stay unchanged).
func localOSRoot(libPath string) string {
	if !strings.Contains(libPath, "://") {
		return libPath
	}
	u, err := url.Parse(libPath)
	if err != nil || u.Scheme != storage.LocalSchemaID {
		return libPath
	}
	// Windows drive URLs (file://C:/Music) put the volume in Host; rejoin it, matching
	// core/storage/local's newLocalStorage so os.Open/os.Stat get a valid path.
	if filepath.VolumeName(u.Host) != "" {
		return filepath.Join(u.Host, u.Path)
	}
	return u.Path
}
