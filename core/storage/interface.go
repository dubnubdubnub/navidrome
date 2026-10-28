package storage

import (
	"context"
	"io/fs"
	"time"

	"github.com/navidrome/navidrome/model/metadata"
)

type Storage interface {
	FS() (MusicFS, error)
}

// MusicFS is an interface that extends the fs.FS interface with the ability to read tags from files
type MusicFS interface {
	fs.FS
	ReadTags(path ...string) (map[string]metadata.Info, error)
}

// SymlinkResolverFS is an optional interface for MusicFS implementations backed by a real
// filesystem. ResolveSymlink resolves the whole symlink chain of the named entry at the OS
// level and returns the final target's path — including targets outside the FS root, which
// fs.ReadLink-based resolution cannot follow.
type SymlinkResolverFS interface {
	ResolveSymlink(name string) (string, error)
}

// URLProvider is an optional interface that a Storage or MusicFS may implement to
// produce a time-limited, directly-fetchable URL (e.g. a presigned S3 GET URL) for
// a library-relative path. Backends whose objects are reachable over HTTP (S3, etc.)
// implement this so consumers that cannot read from an fs.FS — most notably ffmpeg,
// which is a subprocess and takes a path or URL as input — can be handed a URL instead
// of a local file path. The local (file://) backend deliberately does NOT implement
// this, so callers fall back to the on-disk path for local libraries.
type URLProvider interface {
	// PresignedURL returns an HTTP(S) URL that grants read access to the object at the
	// given library-relative path for expiry. The URL is a bearer credential (it carries
	// the signing key ID and a signature): keep it out of logs and errors, and ask for the
	// shortest expiry that covers the read.
	PresignedURL(ctx context.Context, path string, expiry time.Duration) (string, error)
}

// Watcher is a storage with the ability watch the FS and notify changes
type Watcher interface {
	// Start starts a watcher on the whole FS and returns a channel to send detected changes.
	// The watcher must be stopped when the context is done.
	Start(context.Context) (<-chan string, error)
}
