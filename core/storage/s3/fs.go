package s3

import (
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// readAheadSize is the size of the buffer fetched on each underlying ranged GET. Tag
// reads (especially FLAC/embedded art) seek around the head of the file in many small
// reads; a read-ahead buffer collapses those into a handful of HTTP requests.
const readAheadSize = 256 * 1024

// ---- Directory listing / walking ----

// Open implements fs.FS. A name referring to an object returns a seekable file backed by
// ranged GETs; a name referring to a (synthesized) directory returns a directory handle
// that can be ReadDir'd.
func (sfs *s3FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return &s3Dir{sfs: sfs, name: "."}, nil
	}

	// Try to stat it as an object first.
	key := name
	oi, err := sfs.client.StatObject(sfs.ctx(), sfs.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return newS3File(sfs, key, oi.Size, oi.LastModified), nil
	}
	if !isNotFound(err) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	// Not an object: treat as a directory if any object has it as a prefix.
	if sfs.dirExists(name) {
		return &s3Dir{sfs: sfs, name: name}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// Stat implements fs.StatFS.
func (sfs *s3FS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return &s3FileInfo{name: ".", dir: true}, nil
	}
	oi, err := sfs.client.StatObject(sfs.ctx(), sfs.bucket, name, minio.StatObjectOptions{})
	if err == nil {
		return &s3FileInfo{name: path.Base(name), size: oi.Size, modTime: oi.LastModified}, nil
	}
	if !isNotFound(err) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	if sfs.dirExists(name) {
		return &s3FileInfo{name: path.Base(name), dir: true}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

// ReadDir implements fs.ReadDirFS, listing one directory level using a "/" delimiter.
func (sfs *s3FS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	prefix := ""
	if name != "." {
		prefix = name + "/"
	}

	entries := make([]fs.DirEntry, 0, 32)
	seenDirs := make(map[string]bool)

	opts := minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: false, // delimiter "/" behavior
	}
	for obj := range sfs.client.ListObjects(sfs.ctx(), sfs.bucket, opts) {
		if obj.Err != nil {
			return nil, &fs.PathError{Op: "readdir", Path: name, Err: obj.Err}
		}
		rel := strings.TrimPrefix(obj.Key, prefix)
		if rel == "" {
			continue
		}
		if strings.HasSuffix(obj.Key, "/") {
			// Common prefix (synthesized directory).
			dirName := strings.TrimSuffix(rel, "/")
			if dirName == "" || seenDirs[dirName] {
				continue
			}
			seenDirs[dirName] = true
			entries = append(entries, &s3DirEntry{info: &s3FileInfo{name: dirName, dir: true}})
			continue
		}
		// A regular object directly under this prefix.
		entries = append(entries, &s3DirEntry{info: &s3FileInfo{
			name:    rel,
			size:    obj.Size,
			modTime: obj.LastModified,
		}})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

// dirExists reports whether any object exists under the given path as a prefix,
// i.e. whether the path should be treated as a directory.
func (sfs *s3FS) dirExists(name string) bool {
	prefix := name + "/"
	opts := minio.ListObjectsOptions{Prefix: prefix, Recursive: false, MaxKeys: 1}
	for obj := range sfs.client.ListObjects(sfs.ctx(), sfs.bucket, opts) {
		if obj.Err != nil {
			return false
		}
		return true
	}
	return false
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.StatusCode == 404 || resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket"
	}
	return false
}

// ---- FileInfo / DirEntry ----

type s3FileInfo struct {
	name    string
	size    int64
	modTime time.Time
	dir     bool
}

func (fi *s3FileInfo) Name() string { return fi.name }
func (fi *s3FileInfo) Size() int64  { return fi.size }
func (fi *s3FileInfo) Mode() fs.FileMode {
	if fi.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (fi *s3FileInfo) ModTime() time.Time { return fi.modTime }
func (fi *s3FileInfo) IsDir() bool        { return fi.dir }
func (fi *s3FileInfo) Sys() any           { return nil }

type s3DirEntry struct{ info *s3FileInfo }

func (de *s3DirEntry) Name() string               { return de.info.name }
func (de *s3DirEntry) IsDir() bool                { return de.info.dir }
func (de *s3DirEntry) Type() fs.FileMode          { return de.info.Mode().Type() }
func (de *s3DirEntry) Info() (fs.FileInfo, error) { return de.info, nil }

// ---- Directory handle ----

type s3Dir struct {
	sfs     *s3FS
	name    string
	entries []fs.DirEntry
	read    bool
	offset  int
}

func (d *s3Dir) Stat() (fs.FileInfo, error) {
	return &s3FileInfo{name: path.Base(d.name), dir: true}, nil
}

func (d *s3Dir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("is a directory")}
}

func (d *s3Dir) Close() error { return nil }

// ReadDir implements fs.ReadDirFile so walking works on the directory handle returned by Open.
func (d *s3Dir) ReadDir(n int) ([]fs.DirEntry, error) {
	if !d.read {
		entries, err := d.sfs.ReadDir(d.name)
		if err != nil {
			return nil, err
		}
		d.entries = entries
		d.read = true
	}
	if n <= 0 {
		rest := d.entries[d.offset:]
		d.offset = len(d.entries)
		return rest, nil
	}
	if d.offset >= len(d.entries) {
		return nil, io.EOF
	}
	end := d.offset + n
	if end > len(d.entries) {
		end = len(d.entries)
	}
	res := d.entries[d.offset:end]
	d.offset = end
	return res, nil
}
