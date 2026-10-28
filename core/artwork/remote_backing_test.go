package artwork

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/navidrome/navidrome/core/storage/storagetest"
	"github.com/navidrome/navidrome/model"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// A remote (s3://) library has no OS path, so a library-file hit must not persist a
// sourcePath that openOriginal would later os.Open/os.Stat (it can never exist on disk).
var _ = Describe("remote library backing", func() {
	It("keeps sourcePath and refMtime for a local library", func() {
		lib := libraryView{FS: &storagetest.FakeFS{}, absRoot: "/music"}
		Expect(lib.isRemote()).To(BeFalse())
		res := lib.withBacking(resolution{source: "folder", sourcePath: lib.Abs("a/cover.jpg"), refMtime: 42})
		Expect(res.sourcePath).ToNot(BeEmpty())
		Expect(res.refMtime).To(Equal(int64(42)))
	})

	It("drops sourcePath and refMtime for a remote library", func() {
		lib := libraryView{FS: urlProviderFS{&storagetest.FakeFS{}}, absRoot: "s3://music/library"}
		Expect(lib.isRemote()).To(BeTrue())
		for _, source := range []string{"folder", "embedded"} {
			res := lib.withBacking(resolution{source: source, sourcePath: lib.Abs("a/cover.jpg"), refMtime: 42})
			Expect(res.source).To(Equal(source))
			Expect(res.sourcePath).To(BeEmpty())
			Expect(res.refMtime).To(BeZero())
		}
	})

	It("stores a remote folder hit's bytes and serves them back from the store", func() {
		store := NewImageStore(GinkgoT().TempDir())
		data := []byte("not really a jpeg, but the store does not care")
		hash, err := hashImage(bytes.NewReader(data))
		Expect(err).ToNot(HaveOccurred())
		art := &model.Artwork{Hash: hash, Mime: "image/jpeg"}

		sourcePath, refMtime, err := placeBytes(store, art, resolution{source: "folder"}, data)
		Expect(err).ToNot(HaveOccurred())
		Expect(sourcePath).To(BeEmpty())
		Expect(refMtime).To(BeZero())

		r, err := openOriginal(&model.ItemArtwork{Hash: hash, Source: "folder"}, art.Mime, store)
		Expect(err).ToNot(HaveOccurred())
		defer r.Close()
		got, err := io.ReadAll(r)
		Expect(err).ToNot(HaveOccurred())
		Expect(got).To(Equal(data))
	})

	It("still leaves a local folder hit's bytes in the library file", func() {
		store := NewImageStore(GinkgoT().TempDir())
		art := &model.Artwork{Hash: "abc", Mime: "image/jpeg"}
		sourcePath, refMtime, err := placeBytes(store, art, resolution{source: "folder", sourcePath: "/music/a/cover.jpg", refMtime: 7}, []byte("x"))
		Expect(err).ToNot(HaveOccurred())
		Expect(sourcePath).To(Equal("/music/a/cover.jpg"))
		Expect(refMtime).To(Equal(int64(7)))
		_, err = store.Open("abc", "image/jpeg")
		Expect(err).To(HaveOccurred())
	})
})

// urlProviderFS stands in for the s3 backend: a MusicFS that also presigns URLs.
type urlProviderFS struct{ *storagetest.FakeFS }

func (urlProviderFS) PresignedURL(_ context.Context, path string, _ time.Duration) (string, error) {
	return "https://garage.example/" + path, nil
}
