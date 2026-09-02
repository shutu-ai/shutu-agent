package attachment

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/llm"
)

// testPNG returns a small valid PNG fixture used by storage and admission tests.
func testPNG() []byte {
	return testImage("image/png")
}

func testImage(mediaType string) []byte {
	if mediaType == "image/webp" {
		// Minimal valid VP8X container for a 1x1 image. The Go standard
		// library has no WebP decoder, so tests exercise the container and
		// dimension admission parser directly.
		return append([]byte("RIFF\x16\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00"), make([]byte, 10)...)
	}
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 0x20, G: 0x40, B: 0x80, A: 0xff})
	var buf bytes.Buffer
	switch mediaType {
	case "image/jpeg":
		_ = jpeg.Encode(&buf, img, nil)
	case "image/gif":
		_ = gif.Encode(&buf, img, nil)
	default:
		_ = png.Encode(&buf, img)
	}
	return buf.Bytes()
}

// TestNewStoreCreatesDirectory verifies NewStore creates the directory when it
// does not exist (dispatch-m8-3 §2: <dir> 不存在则 mkdir).
func TestNewStoreCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "attachments")
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if st == nil {
		t.Fatal("NewStore returned nil store")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("dir %s not created: err=%v", dir, err)
	}
}

// TestNewStoreDefaultDir verifies an empty dir falls back to
// <data_dir>/attachments (dispatch-m8-3 §2).
func TestNewStoreDefaultDir(t *testing.T) {
	st, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if st == nil {
		t.Fatal("NewStore returned nil store")
	}
}

// TestSaveImageRoundTrip verifies Save then Read returns the identical bytes,
// and the returned ImageRef carries the expected metadata (ID/MediaType/Bytes/
// Width/Height/Path; dimensions are read from the raster header.
func TestSaveImageRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	data := testPNG()
	ref, err := st.SaveImage("image/png", data, 1024)
	if err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	if ref.ID == "" {
		t.Fatal("ImageRef.ID must be non-empty")
	}
	if ref.MediaType != "image/png" {
		t.Errorf("MediaType = %q, want image/png", ref.MediaType)
	}
	if ref.Bytes != int64(len(data)) {
		t.Errorf("Bytes = %d, want %d", ref.Bytes, len(data))
	}
	if ref.Width != 1 || ref.Height != 1 {
		t.Errorf("Width/Height = %d/%d, want 1/1", ref.Width, ref.Height)
	}
	if !filepath.IsAbs(filepath.Clean(ref.Path)) && !strings.HasPrefix(ref.Path, dir) {
		t.Errorf("Path = %q, want under dir %q", ref.Path, dir)
	}
	// The file exists on disk at <dir>/<id>.png with the exact bytes.
	if fi, err := os.Stat(ref.Path); err != nil || fi.Size() != int64(len(data)) {
		t.Errorf("saved file %s: err=%v size=%v", ref.Path, err, fi.Size())
	}
	got, err := st.Read(ref)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("Read bytes differ: got %d bytes, want %d", len(got), len(data))
	}
}

// TestSaveImageContentAddressReuse verifies two saves of the same payload
// reuse one verified content-addressed object.
func TestSaveImageContentAddressReuse(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	data := testPNG()
	r1, err := st.SaveImage("image/png", data, 1024)
	if err != nil {
		t.Fatalf("SaveImage 1: %v", err)
	}
	r2, err := st.SaveImage("image/png", data, 1024)
	if err != nil {
		t.Fatalf("SaveImage 2: %v", err)
	}
	if r1.ID != r2.ID {
		t.Fatalf("content-addressed ids must match, got %q and %q", r1.ID, r2.ID)
	}
	if r1.Path != r2.Path {
		t.Fatalf("content-addressed paths must match, got %q and %q", r1.Path, r2.Path)
	}
}

// TestSaveImageRejectsUnsupportedType verifies fail-closed on an unsupported
// media type (dispatch-m8-3 §5: 坏扩展名 fail-closed). The store only accepts
// the SupportedMediaTypes vocabulary.
func TestSaveImageRejectsUnsupportedType(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := st.SaveImage("image/tiff", testPNG(), 1024); err == nil {
		t.Fatal("unsupported media type must fail closed")
	} else if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("err = %q, want the unsupported-type error", err)
	}
	if _, err := st.SaveImage("", testPNG(), 1024); err == nil {
		t.Fatal("empty media type must fail closed")
	}
}

// TestSaveImageRejectsEmptyData verifies fail-closed on empty data
// (dispatch-m8-3 §5: 空数据 fail-closed).
func TestSaveImageRejectsEmptyData(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := st.SaveImage("image/png", nil, 1024); err == nil {
		t.Fatal("empty data must fail closed")
	} else if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %q, want the empty-data error", err)
	}
}

func TestSaveImageRejectsMalformedDeclaredRaster(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveImage("image/png", []byte("not a png"), 1024); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("malformed image error = %v, want ErrInvalidImage", err)
	}
	if _, err := st.SaveImage("image/jpeg", testPNG(), 1024); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("wrong declared format error = %v, want ErrInvalidImage", err)
	}
}

func TestReadRejectsTamperedContentAddress(t *testing.T) {
	root := t.TempDir()
	st, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := st.SaveImage("image/png", testPNG(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ref.Path, testImage("image/jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Read(ref); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered read error = %v, want digest mismatch", err)
	}
}

func TestReadRejectsPathOutsideAttachmentRoot(t *testing.T) {
	root := t.TempDir()
	st, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, testPNG(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Read(llm.ImageRef{Path: outside, MediaType: "image/png"}); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("outside read error = %v, want root containment error", err)
	}
}

// TestSaveImageRejectsTooLarge verifies fail-closed when data exceeds maxBytes
// (dispatch-m8-3 §5: 超限 fail-closed). A payload exactly at the limit is
// accepted.
func TestSaveImageRejectsTooLarge(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	data := testPNG()
	if _, err := st.SaveImage("image/png", data, 10); err == nil {
		t.Fatal("oversized data must fail closed")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %q, want the too-large error", err)
	}
	// Exactly at the limit: accepted.
	if _, err := st.SaveImage("image/png", data, len(data)); err != nil {
		t.Errorf("data exactly at maxBytes must be accepted: %v", err)
	}
	// maxBytes <= 0 means no size gate (the config default is applied upstream).
	if _, err := st.SaveImage("image/png", data, 0); err != nil {
		t.Errorf("non-positive maxBytes must not gate: %v", err)
	}
}

func TestSaveImagesValidatesWholeBatchBeforeWriting(t *testing.T) {
	root := t.TempDir()
	st, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	refs, err := st.SaveImages([]ImageInput{
		{MediaType: "image/png", Data: testPNG()},
		{MediaType: "image/tiff", Data: testPNG()},
	}, 1024)
	if err == nil || !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("SaveImages invalid batch = refs=%v err=%v, want unsupported type", refs, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid batch wrote %d files, want none", len(entries))
	}
	refs, err = st.SaveImages([]ImageInput{
		{MediaType: "image/png", Data: testPNG()},
		{MediaType: "image/jpeg", Data: testImage("image/jpeg")},
	}, 1024)
	if err != nil || len(refs) != 2 {
		t.Fatalf("SaveImages valid batch = refs=%v err=%v, want two refs", refs, err)
	}
}

func TestSaveImagesRollbackPreservesPreexistingContentAddress(t *testing.T) {
	root := t.TempDir()
	st, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	first := testPNG()
	second := testImage("image/jpeg")
	preexistingPath := filepath.Join(root, imageID(second)+".jpg")
	preexisting := []byte("pre-existing collision marker")
	if err := os.WriteFile(preexistingPath, preexisting, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveImages([]ImageInput{
		{MediaType: "image/png", Data: first},
		{MediaType: "image/jpeg", Data: second},
	}, 1024); err == nil {
		t.Fatal("batch must fail when a content-addressed target is already inconsistent")
	}
	if _, err := os.Stat(filepath.Join(root, imageID(first)+".png")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback left first newly-created file: %v", err)
	}
	got, err := os.ReadFile(preexistingPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, preexisting) {
		t.Fatalf("rollback modified pre-existing file: got %q", got)
	}
}

func TestStoreUsesConfiguredAdmissionLimits(t *testing.T) {
	first := testPNG()
	st, err := NewStoreWithLimits(t.TempDir(), Limits{
		MaxImagesPerMessage:  1,
		MaxMessageImageBytes: len(first) * 2,
		MaxImagePixels:       1,
		MaxImageDimension:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveImages([]ImageInput{
		{MediaType: "image/png", Data: first},
		{MediaType: "image/png", Data: first},
	}, 0); !errors.Is(err, ErrTooManyImages) {
		t.Fatalf("configured image-count limit error = %v, want ErrTooManyImages", err)
	}
	wide := image.NewRGBA(image.Rect(0, 0, 2, 1))
	var buf bytes.Buffer
	if err := png.Encode(&buf, wide); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveImage("image/png", buf.Bytes(), 0); !errors.Is(err, ErrDimensionTooLarge) {
		t.Fatalf("configured dimension limit error = %v, want ErrDimensionTooLarge", err)
	}
}

// TestSaveImageRoundTripForEverySupportedType verifies every SupportedMediaTypes
// entry saves and reads back intact (the ext→media-type map and the
// media-type→ext reverse lookup stay in sync).
func TestSaveImageRoundTripForEverySupportedType(t *testing.T) {
	for ext, mediaType := range SupportedMediaTypes {
		ext, mediaType := ext, mediaType
		t.Run(ext, func(t *testing.T) {
			st, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			data := testImage(mediaType)
			ref, err := st.SaveImage(mediaType, data, 1024)
			if err != nil {
				t.Fatalf("SaveImage(%s): %v", mediaType, err)
			}
			if ref.MediaType != mediaType {
				t.Errorf("MediaType = %q, want %q", ref.MediaType, mediaType)
			}
			if got := MediaTypeForExtension(ext); got != mediaType {
				t.Errorf("MediaTypeForExtension(%q) = %q, want %q", ext, got, mediaType)
			}
			got, err := st.Read(ref)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if string(got) != string(data) {
				t.Errorf("Read bytes differ for %s", ext)
			}
		})
	}
}

// TestMediaTypeForExtensionCaseInsensitive verifies extension lookup is
// case-insensitive and unknown extensions return "".
func TestMediaTypeForExtensionCaseInsensitive(t *testing.T) {
	if got := MediaTypeForExtension(".PNG"); got != "image/png" {
		t.Errorf(".PNG = %q, want image/png", got)
	}
	if got := MediaTypeForExtension(".jpeg"); got != "image/jpeg" {
		t.Errorf(".jpeg = %q, want image/jpeg", got)
	}
	if got := MediaTypeForExtension(".bmp"); got != "" {
		t.Errorf(".bmp = %q, want empty", got)
	}
}

// TestReadMissingPath verifies Read fails closed when the ref's path is missing
// (dispatch-m8-3 §2: Path 缺失/不可读返回错误).
func TestReadMissingPath(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ref, err := st.SaveImage("image/png", testPNG(), 1024)
	if err != nil {
		t.Fatalf("SaveImage: %v", err)
	}
	if err := os.Remove(ref.Path); err != nil {
		t.Fatalf("remove saved file: %v", err)
	}
	if _, err := st.Read(ref); err == nil {
		t.Fatal("Read of a removed file must fail closed")
	}
}
