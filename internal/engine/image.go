package engine

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	"github.com/HugoSmits86/nativewebp"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// An attached image is what the model sees: models from Claude 4.7 on take up to imageEdge pixels on the long edge and imageTokens image
// tokens of 28 by 28 pixels, older ones less, which the API scales to itself. read scales a bigger one down, so nothing rescales it unseen.
const (
	imageEdge   = 2576
	imageTokens = 4784
	// imageBytes is 5 MB once base64-encoded, the smallest per-image limit of the platforms that serve Claude.
	imageBytes = 3_750_000
	// imagePixels bounds what read decodes to scale it, 256 MiB of pixels.
	imagePixels = 8000 * 8000
	// ImageLimit is the largest image file read takes.
	ImageLimit  = 64 * 1024 * 1024
	jpegQuality = 90
)

// imageKind is an image type the model takes: how its bytes start, and its decoders.
type imageKind struct {
	mime string
	// magic are the ways its files start, ? standing for any byte.
	magic  []string
	config func(io.Reader) (image.Config, error)
	decode func(io.Reader) (image.Image, error)
	// lossy marks a type whose files are already lossy, which a scaled copy keeps rather than growing it losslessly.
	lossy bool
}

// imageKinds are the image types the model takes, the one home of that set: what mime sniffs, read scales and attaches.
var imageKinds = []imageKind{
	{mime: "image/png", magic: []string{"\x89PNG\r\n\x1a\n"}, config: png.DecodeConfig, decode: png.Decode},
	{mime: "image/jpeg", magic: []string{"\xff\xd8\xff"}, config: jpeg.DecodeConfig, decode: jpeg.Decode, lossy: true},
	{mime: "image/gif", magic: []string{"GIF87a", "GIF89a"}, config: gif.DecodeConfig, decode: gif.Decode},
	{mime: "image/webp", magic: []string{"RIFF????WEBP"}, config: webp.DecodeConfig, decode: webp.Decode},
}

// starts says whether data starts like a file of the kind.
func (k *imageKind) starts(data []byte) bool {
next:
	for _, magic := range k.magic {
		if len(data) < len(magic) {
			continue
		}
		for i := range len(magic) {
			if magic[i] != '?' && magic[i] != data[i] {
				continue next
			}
		}
		return true
	}
	return false
}

// imageKindOf is the image type data starts like, nil for none.
func imageKindOf(data []byte) *imageKind {
	for i := range imageKinds {
		if imageKinds[i].starts(data) {
			return &imageKinds[i]
		}
	}
	return nil
}

// imageTokenCount is the image tokens a w by h image costs.
func imageTokenCount(w, h int) int { return ((w + 27) / 28) * ((h + 27) / 28) }

// fitImage is the largest size of a w by h image, its aspect kept, within edge pixels on its long side and tokens image tokens.
func fitImage(w, h, edge, tokens int) (int, int) {
	fits := func(w, h int) bool { return max(w, h) <= edge && imageTokenCount(w, h) <= tokens }
	if fits(w, h) {
		return w, h
	}
	at := func(scale float64) (int, int) { return max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale)) }
	lo, hi := 0.0, 1.0
	for range 40 {
		mid := (lo + hi) / 2
		if fits(at(mid)) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return at(lo)
}

// attachImage is what read attaches for an image file's data of kind, nil when it skips one too big to scale, and the note on it. One within
// the model's limits goes as it is; a bigger one goes scaled down, as lossless WebP or, from a lossy file or when that is too big, as JPEG.
func attachImage(display string, data []byte, kind *imageKind) (*Image, string) {
	original := &Image{Data: data, MimeType: kind.mime}
	config, err := kind.config(bytes.NewReader(data))
	if err != nil {
		// One nib cannot read goes as it is, for the model to make of it what it can.
		return original, fmt.Sprintf("%s: %s image attached", display, kind.mime)
	}
	w, h := fitImage(config.Width, config.Height, imageEdge, imageTokens)
	if w == config.Width && h == config.Height && len(data) <= imageBytes {
		return original, fmt.Sprintf("%s: %s %dx%d attached", display, kind.mime, w, h)
	}
	if config.Width*config.Height > imagePixels {
		return nil, fmt.Sprintf("[%s] skipped: %s %dx%d, more than the %d megapixels nib scales; crop it first", display, kind.mime, config.Width, config.Height, imagePixels/1_000_000)
	}
	source, err := kind.decode(bytes.NewReader(data))
	if err != nil {
		return original, fmt.Sprintf("%s: %s image attached", display, kind.mime)
	}
	scaled := image.NewNRGBA(image.Rect(0, 0, w, h))
	if w == config.Width && h == config.Height {
		draw.Draw(scaled, scaled.Bounds(), source, source.Bounds().Min, draw.Src)
	} else {
		xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), source, source.Bounds(), draw.Src, nil)
	}
	encoded, mime, err := encodeImage(scaled, kind.lossy)
	if err != nil {
		return original, fmt.Sprintf("%s: %s image attached", display, kind.mime)
	}
	note := fmt.Sprintf("%s: %s %dx%d attached as %s", display, kind.mime, config.Width, config.Height, mime)
	if w != config.Width || h != config.Height {
		note += fmt.Sprintf(" %dx%d, the most the model sees; crop a part for more detail", w, h)
	}
	return &Image{Data: encoded, MimeType: mime}, note
}

// encodeImage encodes img losslessly as WebP, or as JPEG for a lossy source or when WebP would be over imageBytes.
func encodeImage(img *image.NRGBA, lossy bool) ([]byte, string, error) {
	var out bytes.Buffer
	if !lossy {
		if err := nativewebp.Encode(&out, img, &nativewebp.Options{CompressionLevel: nativewebp.BestSpeed}); err == nil && out.Len() <= imageBytes {
			return out.Bytes(), "image/webp", nil
		}
		out.Reset()
	}
	// JPEG has no alpha, so transparent parts show the white a viewer would put under them, not black.
	flat := image.NewRGBA(img.Bounds())
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), img, image.Point{}, draw.Over)
	if err := jpeg.Encode(&out, flat, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", err
	}
	return out.Bytes(), "image/jpeg", nil
}
