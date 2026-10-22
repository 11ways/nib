package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/webp"
)

func TestFitImageKeepsTheAspectWithinTheEdgeAndTokens(t *testing.T) {
	// The examples of Anthropic's vision guide: an A4 page and a 1080p screenshot on the standard tier, 4K on the high-resolution one.
	for _, c := range []struct{ w, h, edge, tokens, wantW, wantH int }{
		{1075, 1520, 1568, 1568, 924, 1307},
		{1920, 1080, 1568, 1568, 1456, 819},
		{1000, 1000, imageEdge, imageTokens, 1000, 1000},
		{1400, 17724, imageEdge, imageTokens, 203, 2576},
	} {
		w, h := fitImage(c.w, c.h, c.edge, c.tokens)
		if w != c.wantW || h != c.wantH {
			t.Errorf("%dx%d: got %dx%d, want %dx%d", c.w, c.h, w, h, c.wantW, c.wantH)
		}
	}
	w, h := fitImage(3840, 2160, imageEdge, imageTokens)
	if max(w, h) > imageEdge || imageTokenCount(w, h) > imageTokens || imageTokenCount(w+1, h+1) <= imageTokens && max(w+1, h+1) <= imageEdge {
		t.Errorf("4K: %dx%d is not the largest that fits", w, h)
	}
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// gradient is an image that compresses well, like a screenshot's flat areas.
func gradient(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	return img
}

func TestAttachImageScalesOnlyWhatTheModelCouldNotTake(t *testing.T) {
	png := imageKindOf([]byte("\x89PNG\r\n\x1a\n"))
	// Within the limits the file goes as it is.
	small := encodePNG(t, gradient(300, 200))
	attached, note := attachImage("a.png", small, png)
	if !bytes.Equal(attached.Data, small) || attached.MimeType != "image/png" || note != "a.png: image/png 300x200 attached" {
		t.Fatalf("got %s, %s", attached.MimeType, note)
	}
	// A taller one is scaled to the long edge, losslessly as WebP.
	tall := gradient(200, 3000)
	attached, note = attachImage("tall.png", encodePNG(t, tall), png)
	if attached.MimeType != "image/webp" || note != "tall.png: image/png 200x3000 attached as image/webp 171x2576, the most the model sees; crop a part for more detail" {
		t.Fatalf("got %s, %s", attached.MimeType, note)
	}
	decoded, err := webp.Decode(bytes.NewReader(attached.Data))
	if err != nil || decoded.Bounds().Dx() != 171 || decoded.Bounds().Dy() != 2576 {
		t.Fatalf("decoded %v, %v", decoded.Bounds(), err)
	}
	// A JPEG stays JPEG, which a photo is smaller as.
	var photo bytes.Buffer
	if err := jpeg.Encode(&photo, gradient(3000, 1000), nil); err != nil {
		t.Fatal(err)
	}
	attached, note = attachImage("photo.jpg", photo.Bytes(), imageKindOf(photo.Bytes()))
	if attached.MimeType != "image/jpeg" || !strings.HasPrefix(note, "photo.jpg: image/jpeg 3000x1000 attached as image/jpeg 2576x858,") {
		t.Fatalf("got %s, %s", attached.MimeType, note)
	}
	// A file nib cannot read goes as it is.
	header := []byte("\x89PNG\r\n\x1a\n")
	attached, note = attachImage("pixel.png", header, png)
	if !bytes.Equal(attached.Data, header) || note != "pixel.png: image/png image attached" {
		t.Fatalf("got %s", note)
	}
}

func TestAttachImageReencodesOneTooBigInBytesAndSkipsOneTooBigToScale(t *testing.T) {
	png := imageKindOf([]byte("\x89PNG\r\n\x1a\n"))
	// Noise compresses in neither PNG nor lossless WebP, so it fits the pixels but not the bytes, and goes as JPEG.
	noise := image.NewNRGBA(image.Rect(0, 0, 1100, 1100))
	random := rand.New(rand.NewPCG(1, 2))
	for i := range noise.Pix {
		noise.Pix[i] = uint8(random.Uint32())
	}
	data := encodePNG(t, noise)
	if len(data) <= imageBytes {
		t.Fatalf("noise is only %d bytes", len(data))
	}
	attached, note := attachImage("noise.png", data, png)
	if attached.MimeType != "image/jpeg" || len(attached.Data) > imageBytes || note != "noise.png: image/png 1100x1100 attached as image/jpeg" {
		t.Fatalf("got %s of %d bytes, %s", attached.MimeType, len(attached.Data), note)
	}
	// Past the pixels nib decodes, the header alone says so.
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 9000)
	binary.BigEndian.PutUint32(ihdr[4:], 9000)
	ihdr[8], ihdr[9] = 8, 2
	chunk := binary.BigEndian.AppendUint32(nil, 13)
	chunk = append(chunk, "IHDR"...)
	chunk = append(chunk, ihdr...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	huge := append([]byte("\x89PNG\r\n\x1a\n"), chunk...)
	attached, note = attachImage("huge.png", huge, png)
	if attached != nil || note != "[huge.png] skipped: image/png 9000x9000, more than the 64 megapixels nib scales; crop it first" {
		t.Fatalf("got %v, %s", attached != nil, note)
	}
}

func TestReadTakesAnImageBiggerThanTextAndAttachesWhatTheModelSees(t *testing.T) {
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	// Noise past what a text read holds, wider than the model's edge.
	noise := image.NewNRGBA(image.Rect(0, 0, 3000, 800))
	random := rand.New(rand.NewPCG(3, 4))
	for i := range noise.Pix {
		noise.Pix[i] = uint8(random.Uint32())
	}
	data := encodePNG(t, noise)
	if len(data) <= TextLimit {
		t.Fatalf("only %d bytes", len(data))
	}
	if err := os.WriteFile(filepath.Join(directory, "shot.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := New().Read(&Context{Ctx: context.Background(), Directory: directory, Session: "s"}, ReadArgs{Paths: []string{"shot.png"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Images) != 1 || len(read.Images[0].Data) > imageBytes || !strings.HasPrefix(read.Text, "shot.png: image/png 3000x800 attached as image/") {
		t.Fatalf("got %d images: %s", len(read.Images), read.Text)
	}
}

func TestMimeSniffsEveryImageKind(t *testing.T) {
	for data, want := range map[string]string{
		"\x89PNG\r\n\x1a\nrest":        "image/png",
		"\xff\xd8\xff\xe0":             "image/jpeg",
		"GIF87a..":                     "image/gif",
		"GIF89a..":                     "image/gif",
		"RIFF\x00\x00\x00\x00WEBPVP8L": "image/webp",
		"RIFF\x00\x00\x00\x00WAVE":     "",
		"%PDF-1.7":                     "application/pdf",
		"GIF88a":                       "",
	} {
		if got := mime([]byte(data)); got != want {
			t.Errorf("%q: got %q, want %q", data, got, want)
		}
	}
}
