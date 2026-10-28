package artwork

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/navidrome/navidrome/core/storage"
	"github.com/navidrome/navidrome/model"
)

// libraryView bundles the MusicFS for a library with its absolute root path,
// so readers can open library-relative paths through FS and compose absolute
// paths (for ffmpeg, which is path-based) via Abs.
type libraryView struct {
	FS      storage.MusicFS
	absRoot string
	// remote is true for libraries whose files are not on the local filesystem (s3://, see
	// isRemoteFS). Their paths cannot be os.Open'd/os.Stat'd later, so the artwork store
	// must not record them as a backing file; see localPath.
	remote bool
}

// Abs returns the absolute path for a library-relative path. Returns "" for an
// empty rel so callers (fromFFmpegTag) can treat it as "no path available".
func (v libraryView) Abs(rel string) string {
	if rel == "" {
		return ""
	}
	return filepath.Join(v.absRoot, rel)
}

// isRemoteFS reports whether a library's files live behind a URL rather than on the local
// filesystem. The URLProvider capability is the marker: it is what s3:// implements so ffmpeg
// can be handed a presigned URL, and local backends deliberately do not implement it.
func isRemoteFS(fsys storage.MusicFS) bool {
	_, ok := fsys.(storage.URLProvider)
	return ok
}

// localPath returns the on-disk path the artwork store may later os.Open/os.Stat for a
// library-relative path, or "" when the library is not on the local filesystem (s3://).
// An empty result makes the resolution store-backed: its bytes are copied into the image
// store and there is no backing file whose mtime could be re-checked.
func (v libraryView) localPath(rel string) string {
	if v.remote {
		return ""
	}
	return v.Abs(rel)
}

// ffmpegInput returns the input ffmpeg should read for the given library-relative path:
// a presigned HTTP URL when the library's backend supports it (s3://), otherwise the
// absolute on-disk path (file://). Returns "" for an empty rel so callers can treat it
// as "no input available" and fall through.
func (v libraryView) ffmpegInput(ctx context.Context, rel string) string {
	if rel == "" {
		return ""
	}
	if up, ok := v.FS.(storage.URLProvider); ok {
		if u, err := up.PresignedURL(ctx, rel); err == nil {
			return u
		}
		// Presigning failed; nothing readable for a remote backend (no local path
		// exists), so signal "no input" rather than handing ffmpeg a bogus path.
		return ""
	}
	return v.Abs(rel)
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
	return libraryView{FS: fs, absRoot: localOSRoot(lib.Path), remote: isRemoteFS(fs)}, nil
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
