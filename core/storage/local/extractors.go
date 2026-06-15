package local

import (
	"io/fs"
	"sync"

	"github.com/navidrome/navidrome/model/metadata"
)

// Extractor is an interface that defines the methods that a tag/metadata extractor must implement
type Extractor interface {
	Parse(files ...string) (map[string]metadata.Info, error)
	Version() string
}

type extractorConstructor func(fs.FS, string) Extractor

var (
	extractors = map[string]extractorConstructor{}
	lock       sync.RWMutex
)

// RegisterExtractor registers a new extractor, so it can be used by the local storage. The one to be used is
// defined with the configuration option Scanner.Extractor.
func RegisterExtractor(id string, f extractorConstructor) {
	lock.Lock()
	defer lock.Unlock()
	extractors[id] = f
}

// Extractor is constructed from an fs.FS and a base directory. Renamed type alias kept
// here so other storage backends (e.g. s3) can build the same extractors against their
// own fs.FS implementation.
type ExtractorConstructor = extractorConstructor

// GetExtractor returns the registered extractor constructor for the given id. The
// boolean is false if no extractor is registered under that id. This lets non-local
// storage backends reuse the same tag extractors (e.g. gotaglib) against their fs.FS.
func GetExtractor(id string) (ExtractorConstructor, bool) {
	lock.RLock()
	defer lock.RUnlock()
	f, ok := extractors[id]
	return f, ok
}
