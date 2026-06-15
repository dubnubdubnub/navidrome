package s3

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"time"

	"github.com/minio/minio-go/v7"
)

// s3File is an fs.File over a single S3 object that also implements io.ReadSeeker. Tag
// extraction (gotaglib) requires a ReadSeeker, and tends to perform many small reads and
// seeks near the head of the file. To avoid issuing a separate HTTP GET per read, reads
// are served from an in-memory read-ahead buffer that is (re)filled with a single ranged
// GET whenever the requested offset falls outside the current buffer.
type s3File struct {
	sfs     *s3FS
	key     string
	size    int64
	modTime time.Time

	pos int64 // logical read/seek position

	buf      []byte
	bufStart int64 // file offset of buf[0]
	bufvalid bool

	closed bool
}

var (
	_ fs.File       = (*s3File)(nil)
	_ io.ReadSeeker = (*s3File)(nil)
)

func newS3File(sfs *s3FS, key string, size int64, modTime time.Time) *s3File {
	return &s3File{sfs: sfs, key: key, size: size, modTime: modTime}
}

func (f *s3File) Stat() (fs.FileInfo, error) {
	return &s3FileInfo{name: path.Base(f.key), size: f.size, modTime: f.modTime}, nil
}

func (f *s3File) Read(p []byte) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if f.pos >= f.size {
		return 0, io.EOF
	}
	if err := f.ensureBuffered(f.pos); err != nil {
		return 0, err
	}
	bufOff := int(f.pos - f.bufStart)
	n := copy(p, f.buf[bufOff:])
	f.pos += int64(n)
	return n, nil
}

func (f *s3File) Seek(offset int64, whence int) (int64, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.pos + offset
	case io.SeekEnd:
		abs = f.size + offset
	default:
		return 0, fmt.Errorf("s3: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("s3: negative seek position %d", abs)
	}
	f.pos = abs
	return abs, nil
}

// ensureBuffered makes sure f.buf contains the byte at file offset `at`.
func (f *s3File) ensureBuffered(at int64) error {
	if f.bufvalidAt(at) {
		return nil
	}
	length := int64(readAheadSize)
	if at+length > f.size {
		length = f.size - at
	}
	if length <= 0 {
		return io.EOF
	}
	data, err := f.rangeGet(at, length)
	if err != nil {
		return err
	}
	f.buf = data
	f.bufStart = at
	f.bufvalid = true
	return nil
}

func (f *s3File) bufvalidAt(at int64) bool {
	return f.bufvalid && at >= f.bufStart && at < f.bufStart+int64(len(f.buf))
}

func (f *s3File) rangeGet(off, length int64) ([]byte, error) {
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(off, off+length-1); err != nil {
		return nil, fmt.Errorf("s3: setting range %d-%d: %w", off, off+length-1, err)
	}
	obj, err := f.sfs.client.GetObject(f.sfs.ctx(), f.sfs.bucket, f.key, opts)
	if err != nil {
		return nil, fmt.Errorf("s3: GetObject %s: %w", f.key, err)
	}
	defer obj.Close()
	data := make([]byte, length)
	n, err := io.ReadFull(obj, data)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, fmt.Errorf("s3: reading range of %s: %w", f.key, err)
	}
	return data[:n], nil
}

func (f *s3File) Close() error {
	f.closed = true
	f.buf = nil
	f.bufvalid = false
	return nil
}
