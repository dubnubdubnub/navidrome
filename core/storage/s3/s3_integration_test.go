package s3_test

import (
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/navidrome/navidrome/adapters/gotaglib" // register the taglib extractor
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core/storage"
	_ "github.com/navidrome/navidrome/core/storage/s3" // register the s3 backend
)

// TestS3Integration scans a real S3-compatible bucket over the native s3:// backend and
// reports a catalog summary. It is skipped unless ND_S3_ENDPOINT is set, so it does not
// run in environments without access to the test bucket.
//
// To run against the Garage test bucket:
//
//	ND_S3_ENDPOINT=http://10.1.1.5:3900 ND_S3_REGION=garage ND_S3_PATHSTYLE=true \
//	ND_S3_ACCESSKEY=... ND_S3_SECRETKEY=... \
//	go test ./core/storage/s3/ -run TestS3Integration -v -count=1
func TestS3Integration(t *testing.T) {
	if os.Getenv("ND_S3_ENDPOINT") == "" {
		t.Skip("ND_S3_ENDPOINT not set; skipping S3 integration test")
	}
	conf.Server.Scanner.Extractor = "taglib"

	bucket := os.Getenv("ND_S3_BUCKET")
	if bucket == "" {
		bucket = "music"
	}

	st, err := storage.For("s3://" + bucket)
	if err != nil {
		t.Fatalf("storage.For: %v", err)
	}
	musicFS, err := st.FS()
	if err != nil {
		t.Fatalf("FS(): %v", err)
	}

	// Walk the whole bucket, collecting audio files and cover-art files.
	var audioFiles []string
	albums := map[string]bool{}
	var coverFiles []string
	start := time.Now()
	err = fs.WalkDir(musicFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		lower := strings.ToLower(p)
		switch {
		case strings.HasSuffix(lower, ".flac") || strings.HasSuffix(lower, ".mp3") ||
			strings.HasSuffix(lower, ".m4a") || strings.HasSuffix(lower, ".ogg"):
			audioFiles = append(audioFiles, p)
			albums[path.Dir(p)] = true
		case isCoverName(path.Base(lower)):
			coverFiles = append(coverFiles, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	walkDur := time.Since(start)

	t.Logf("WALK: %d audio files, %d albums, %d cover-art files in %s",
		len(audioFiles), len(albums), len(coverFiles), walkDur.Round(time.Millisecond))

	if len(audioFiles) == 0 {
		t.Fatal("no audio files found in bucket")
	}

	// Read tags from a sample of files spread across the catalog and report how many
	// have artist/album/title populated and how many have embedded pictures.
	sort.Strings(audioFiles)
	sample := sampleEvenly(audioFiles, 40)
	tagStart := time.Now()
	infos, err := musicFS.ReadTags(sample...)
	if err != nil {
		t.Logf("ReadTags returned error (continuing with partial results): %v", err)
	}
	tagDur := time.Since(tagStart)

	var withArtist, withAlbum, withTitle, withPicture int
	for _, p := range sample {
		info, ok := infos[p]
		if !ok {
			continue
		}
		if firstTag(info.Tags, "artist", "albumartist") != "" {
			withArtist++
		}
		if firstTag(info.Tags, "album") != "" {
			withAlbum++
		}
		if firstTag(info.Tags, "title") != "" {
			withTitle++
		}
		if info.HasPicture {
			withPicture++
		}
	}

	t.Logf("TAGS (sample of %d, read in %s): artist=%d album=%d title=%d embeddedPicture=%d",
		len(sample), tagDur.Round(time.Millisecond), withArtist, withAlbum, withTitle, withPicture)

	// Print a few example rows for eyeballing.
	for i, p := range sample {
		if i >= 5 {
			break
		}
		info := infos[p]
		t.Logf("  %s -> artist=%q album=%q title=%q codec=%q dur=%s pic=%v",
			p,
			firstTag(info.Tags, "artist", "albumartist"),
			firstTag(info.Tags, "album"),
			firstTag(info.Tags, "title"),
			info.AudioProperties.Codec,
			info.AudioProperties.Duration,
			info.HasPicture,
		)
	}

	// Sanity assertions: the sample should mostly have real tags.
	if withTitle == 0 || withAlbum == 0 || withArtist == 0 {
		t.Errorf("expected most sampled files to have artist/album/title tags; got artist=%d album=%d title=%d",
			withArtist, withAlbum, withTitle)
	}
	if withPicture == 0 && len(coverFiles) == 0 {
		t.Errorf("expected some embedded artwork or cover.* files; found neither")
	}
}

func isCoverName(base string) bool {
	if !(strings.HasSuffix(base, ".jpg") || strings.HasSuffix(base, ".jpeg") ||
		strings.HasSuffix(base, ".png") || strings.HasSuffix(base, ".webp") ||
		strings.HasSuffix(base, ".gif")) {
		return false
	}
	name := strings.TrimSuffix(base, path.Ext(base))
	switch name {
	case "cover", "folder", "front", "albumart", "album", "artwork":
		return true
	}
	// Many libraries just have a single image per album folder; count any image.
	return true
}

func sampleEvenly(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	step := len(items) / n
	out := make([]string, 0, n)
	for i := 0; i < len(items) && len(out) < n; i += step {
		out = append(out, items[i])
	}
	return out
}

func firstTag(tags map[string][]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := tags[k]; ok && len(v) > 0 && v[0] != "" {
			return v[0]
		}
	}
	return ""
}
