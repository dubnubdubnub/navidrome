package artwork

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing/fstest"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/conf/configtest"
	"github.com/navidrome/navidrome/core/agents"
	"github.com/navidrome/navidrome/core/storage"
	"github.com/navidrome/navidrome/core/storage/storagetest"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/tests"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// remoteFakeFS stands in for the s3 backend: an in-memory MusicFS that also implements
// storage.URLProvider, the capability that marks a library as not on the local filesystem.
type remoteFakeFS struct{ *storagetest.FakeFS }

func (remoteFakeFS) PresignedURL(_ context.Context, p string) (string, error) {
	return "http://presigned.invalid/" + p, nil
}

type remoteFakeStorage struct{ fs remoteFakeFS }

func (s remoteFakeStorage) FS() (storage.MusicFS, error) { return s.fs, nil }

// remoteTestScheme is distinct from the "fake" scheme other specs register, so the global
// storage registry entry is never swapped out from under them.
const remoteTestScheme = "remotefake"

var _ = Describe("remote (s3-like) library artwork", func() {
	var (
		ctx        context.Context
		ds         *tests.MockDataStore
		folderRepo *fakeFolderRepo
		artRepo    *tests.MockArtworkRepo
		store      *ImageStore
		proc       *processor
		coverBytes []byte
		mp3Bytes   []byte
	)

	BeforeEach(func() {
		DeferCleanup(configtest.SetupConfig())
		ctx = context.Background()
		repoRoot, err := os.Getwd()
		Expect(err).ToNot(HaveOccurred())
		coverBytes, err = os.ReadFile(filepath.Join(repoRoot, "tests/fixtures/artist/an-album/cover.jpg"))
		Expect(err).ToNot(HaveOccurred())
		mp3Bytes, err = os.ReadFile(filepath.Join(repoRoot, "tests/fixtures/artist/an-album/test.mp3"))
		Expect(err).ToNot(HaveOccurred())

		ffs := &storagetest.FakeFS{}
		ffs.SetFiles(fstest.MapFS{
			"artist/an-album/cover.jpg": {Data: coverBytes, ModTime: time.Now()},
			"artist/an-album/test.mp3":  {Data: mp3Bytes, ModTime: time.Now()},
		})
		rfs := remoteFakeFS{ffs}
		storage.Register(remoteTestScheme, func(url.URL) storage.Storage { return remoteFakeStorage{fs: rfs} })

		folderRepo = &fakeFolderRepo{}
		libRepo := &tests.MockLibraryRepo{}
		libRepo.SetData(model.Libraries{{ID: 0, Path: remoteTestScheme + "://bucket"}})
		artRepo = tests.CreateMockArtworkRepo()
		ds = &tests.MockDataStore{MockedFolder: folderRepo, MockedLibrary: libRepo, MockedArtwork: artRepo}
		ds.MockedAlbum = tests.CreateMockAlbumRepo()
		store = NewImageStore(GinkgoT().TempDir())
		ag := agents.GetAgents(&tests.MockDataStore{}, nil)
		proc = &processor{ds: ds, store: store, resolver: newResolver(ds, ag, tests.NewMockFFmpeg(""), nil)}
	})

	It("marks the library view remote, with no local path and a presigned ffmpeg input", func() {
		lib, err := loadLibraryView(ctx, ds, 0)
		Expect(err).ToNot(HaveOccurred())
		Expect(lib.remote).To(BeTrue())
		Expect(lib.localPath("artist/an-album/test.mp3")).To(BeEmpty())
		Expect(lib.ffmpegInput(ctx, "artist/an-album/test.mp3")).To(Equal("http://presigned.invalid/artist/an-album/test.mp3"))
	})

	It("folder art is copied into the store and records no backing file", func() {
		conf.Server.CoverArtPriority = "cover.jpg"
		folderRepo.result = []model.Folder{{Path: "artist/an-album", ImageFiles: []string{"cover.jpg"}}}
		ds.MockedAlbum.(*tests.MockAlbumRepo).SetData(model.Albums{{ID: "al1", Name: "Album", FolderIDs: []string{"f1"}}})

		out, _, _ := proc.acquire(ctx, model.ArtworkQueueItem{ItemKind: "al", ItemID: "al1"})
		Expect(out).To(Equal(outcomeFound))

		ia, err := artRepo.GetItemArtwork(model.KindAlbumArtwork, "al1", model.ImageTypePrimary)
		Expect(err).ToNot(HaveOccurred())
		Expect(ia.Source).To(Equal("folder"))
		Expect(ia.SourcePath).To(BeEmpty(), "an s3 key must never be recorded as an os.Open-able path")
		Expect(ia.RefMtime).To(BeZero())

		art, err := artRepo.GetImage(ia.Hash)
		Expect(err).ToNot(HaveOccurred())
		rc, err := openOriginal(ia, art.Mime, store)
		Expect(err).ToNot(HaveOccurred(), "serving must come from the store, not os.Open of a remote path")
		defer rc.Close()
		served, err := io.ReadAll(rc)
		Expect(err).ToNot(HaveOccurred())
		Expect(served).To(Equal(coverBytes))
	})

	It("embedded art records no backing file, so serving never os.Stats a remote path", func() {
		conf.Server.CoverArtPriority = "embedded"
		folderRepo.result = nil
		ds.MockedAlbum.(*tests.MockAlbumRepo).SetData(model.Albums{
			{ID: "al2", Name: "Album", EmbedArtPath: "artist/an-album/test.mp3", FolderIDs: []string{"f1"}},
		})

		out, _, _ := proc.acquire(ctx, model.ArtworkQueueItem{ItemKind: "al", ItemID: "al2"})
		Expect(out).To(Equal(outcomeFound))

		ia, err := artRepo.GetItemArtwork(model.KindAlbumArtwork, "al2", model.ImageTypePrimary)
		Expect(err).ToNot(HaveOccurred())
		Expect(ia.Source).To(Equal("embedded"))
		Expect(ia.SourcePath).To(BeEmpty())
		Expect(ia.RefMtime).To(BeZero())

		art, err := artRepo.GetImage(ia.Hash)
		Expect(err).ToNot(HaveOccurred())
		rc, err := openOriginal(ia, art.Mime, store)
		Expect(err).ToNot(HaveOccurred())
		rc.Close()
	})

	It("a local library keeps recording its backing file", func() {
		lib := libraryView{FS: osDirFS{os.DirFS(".")}, absRoot: "/music"}
		Expect(lib.remote).To(BeFalse())
		Expect(filepath.ToSlash(lib.localPath("a/cover.jpg"))).To(Equal("/music/a/cover.jpg"))
	})
})

var _ = Describe("openOriginal with an empty file-backed SourcePath", func() {
	It("serves the store copy instead of os.Open(\"\")", func() {
		store := NewImageStore(GinkgoT().TempDir())
		data := []byte("not-really-a-jpeg")
		hash, err := hashImage(bytes.NewReader(data))
		Expect(err).ToNot(HaveOccurred())
		Expect(store.Write(hash, "image/jpeg", bytes.NewReader(data))).To(Succeed())
		rc, err := openOriginal(&model.ItemArtwork{Source: "folder", Hash: hash}, "image/jpeg", store)
		Expect(err).ToNot(HaveOccurred())
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		Expect(got).To(Equal(data))
	})
})
