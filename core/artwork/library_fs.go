package artwork

import (
	"context"
	"path/filepath"

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
	return libraryView{FS: fs, absRoot: lib.Path}, nil
}
