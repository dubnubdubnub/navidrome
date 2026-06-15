// Package s3 implements a native S3-backed storage.Storage for Navidrome, allowing
// the music library to be read directly from S3-compatible object storage (AWS S3,
// MinIO, Garage, ...) without a FUSE mount.
//
// The library path is configured as an s3:// URI, e.g. "s3://music" (the host part of
// the URI is the bucket name). Endpoint, region, credentials and addressing style are
// taken from Navidrome configuration (the S3.* options) with environment-variable
// fallbacks so the backend can be configured without a full config file:
//
//	ND_S3_ENDPOINT, ND_S3_REGION, ND_S3_ACCESSKEY, ND_S3_SECRETKEY,
//	ND_S3_PATHSTYLE, ND_S3_USESSL, and the standard AWS_ACCESS_KEY_ID /
//	AWS_SECRET_ACCESS_KEY for credentials.
//
// This package only covers scanning + tag/embedded-cover-art reading (Phase 4a). It
// reuses the registered tag extractor (gotaglib by default) against the S3 fs.FS, since
// that extractor reads from an io.ReadSeeker and needs no local path.
package s3

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/storage"
	"github.com/navidrome/navidrome/core/storage/local"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model/metadata"
)

// s3Storage implements storage.Storage backed by an S3-compatible bucket.
type s3Storage struct {
	u      url.URL
	bucket string

	once   sync.Once
	client *minio.Client
	initFS *s3FS
	initSt error
}

func newS3Storage(u url.URL) storage.Storage {
	// Determine the bucket. For "s3://music" the bucket is the URL host. For
	// "s3:///music" (used by some configs/tests) it ends up in the path.
	bucket := u.Host
	if bucket == "" {
		bucket = strings.Trim(u.Path, "/")
	}
	return &s3Storage{u: u, bucket: bucket}
}

func (s *s3Storage) connect() (*minio.Client, error) {
	cfg := loadConfig()
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("s3: no endpoint configured (set S3.Endpoint or ND_S3_ENDPOINT)")
	}
	if s.bucket == "" {
		return nil, fmt.Errorf("s3: no bucket in library path %q", s.u.String())
	}

	host, useSSL := normalizeEndpoint(cfg.Endpoint, cfg.UseSSL)
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: useSSL,
		Region: cfg.Region,
	}
	if cfg.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	client, err := minio.New(host, opts)
	if err != nil {
		return nil, fmt.Errorf("s3: creating client: %w", err)
	}
	return client, nil
}

func (s *s3Storage) FS() (storage.MusicFS, error) {
	s.once.Do(func() {
		client, err := s.connect()
		if err != nil {
			s.initSt = err
			return
		}
		s.client = client
		sfs := &s3FS{client: client, bucket: s.bucket}
		// Build the tag extractor against this fs.FS, mirroring local storage's
		// selection logic (fall back to the default if the configured one is missing).
		id := conf.Server.Scanner.Extractor
		ctor, ok := local.GetExtractor(id)
		if !ok || ctor == nil {
			if id != "" && id != consts.DefaultScannerExtractor {
				log.Warn("S3: Extractor not found, using default", "extractor", id, "default", consts.DefaultScannerExtractor)
			}
			ctor, _ = local.GetExtractor(consts.DefaultScannerExtractor)
		}
		if ctor == nil {
			s.initSt = fmt.Errorf("s3: no tag extractor registered (default %q missing)", consts.DefaultScannerExtractor)
			return
		}
		sfs.extractor = ctor(sfs, "s3://"+s.bucket)
		s.initFS = sfs
	})
	if s.initSt != nil {
		return nil, s.initSt
	}
	return s.initFS, nil
}

// s3Config holds resolved connection settings.
type s3Config struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool
	UseSSL    bool
}

func loadConfig() s3Config {
	c := s3Config{
		Endpoint:  conf.Server.S3.Endpoint,
		Region:    conf.Server.S3.Region,
		AccessKey: conf.Server.S3.AccessKey,
		SecretKey: conf.Server.S3.SecretKey,
		PathStyle: conf.Server.S3.PathStyle,
		UseSSL:    conf.Server.S3.UseSSL,
	}
	// Environment fallbacks (useful for test harnesses / containers).
	c.Endpoint = firstNonEmpty(c.Endpoint, os.Getenv("ND_S3_ENDPOINT"))
	c.Region = firstNonEmpty(c.Region, os.Getenv("ND_S3_REGION"))
	c.AccessKey = firstNonEmpty(c.AccessKey, os.Getenv("ND_S3_ACCESSKEY"), os.Getenv("AWS_ACCESS_KEY_ID"))
	c.SecretKey = firstNonEmpty(c.SecretKey, os.Getenv("ND_S3_SECRETKEY"), os.Getenv("AWS_SECRET_ACCESS_KEY"))
	if !c.PathStyle {
		c.PathStyle = parseBoolEnv("ND_S3_PATHSTYLE")
	}
	if !c.UseSSL {
		c.UseSSL = parseBoolEnv("ND_S3_USESSL")
	}
	return c
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseBoolEnv(key string) bool {
	v := os.Getenv(key)
	if v == "" {
		return false
	}
	b, _ := strconv.ParseBool(v)
	return b
}

// normalizeEndpoint strips a scheme from the endpoint (minio wants host:port only) and
// infers SSL from the scheme if present.
func normalizeEndpoint(endpoint string, defaultSSL bool) (host string, useSSL bool) {
	useSSL = defaultSSL
	host = endpoint
	if strings.Contains(endpoint, "://") {
		if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
			host = u.Host
			useSSL = u.Scheme == "https"
		}
	}
	return host, useSSL
}

// s3FS implements storage.MusicFS over a single S3 bucket.
type s3FS struct {
	client    *minio.Client
	bucket    string
	extractor local.Extractor
}

var (
	_ storage.MusicFS = (*s3FS)(nil)
	_ fs.ReadDirFS    = (*s3FS)(nil)
	_ fs.StatFS       = (*s3FS)(nil)
)

func (sfs *s3FS) ReadTags(paths ...string) (map[string]metadata.Info, error) {
	res, err := sfs.extractor.Parse(paths...)
	if err != nil {
		return nil, err
	}
	for p, v := range res {
		if v.FileInfo == nil {
			info, err := sfs.Stat(p)
			if err != nil {
				return nil, err
			}
			v.FileInfo = s3FileInfoWrapper{info}
			res[p] = v
		}
	}
	return res, nil
}

// s3FileInfoWrapper adds a BirthTime method so the info satisfies metadata.FileInfo.
type s3FileInfoWrapper struct {
	fs.FileInfo
}

func (w s3FileInfoWrapper) BirthTime() time.Time {
	return w.FileInfo.ModTime()
}

func (sfs *s3FS) ctx() context.Context { return context.Background() }

func init() {
	storage.Register("s3", newS3Storage)
}
