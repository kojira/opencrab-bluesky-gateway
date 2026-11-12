package bsky

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"

	// Decoders for the formats an image post accepts.
	_ "image/gif"
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// MaxImageBytes is the blob size an image embed accepts: the lexicon's maxSize for
// app.bsky.embed.images#image.image is 2,000,000 bytes ("May be up to 2 MB, formerly limited to
// 1 MB"), less a small margin.
const MaxImageBytes = 1_950_000

// MaxImageEdge is the long edge an oversized image is first scaled down to.
const MaxImageEdge = 2000

var jpegQualities = []int{90, 80, 70, 60}

// PrepareImage makes img fit an image embed. An image under MaxImageBytes is kept as it is; a
// larger one is scaled down (long edge at most MaxImageEdge, aspect ratio kept) and re-encoded as
// JPEG at lower qualities, scaling down further until it fits. It also gives the image's width
// and height for the embed's aspect ratio.
func PrepareImage(img Image) (Image, int, int, error) {
	if len(img.Data) <= MaxImageBytes {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(img.Data))
		if err != nil {
			return Image{}, 0, 0, err
		}
		return img, cfg.Width, cfg.Height, nil
	}
	src, _, err := image.Decode(bytes.NewReader(img.Data))
	if err != nil {
		return Image{}, 0, 0, err
	}
	edge := MaxImageEdge
	for edge >= 100 {
		scaled := scaleDown(src, edge)
		for _, q := range jpegQualities {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: q}); err != nil {
				return Image{}, 0, 0, err
			}
			if buf.Len() <= MaxImageBytes {
				b := scaled.Bounds()
				return Image{Data: buf.Bytes(), MimeType: "image/jpeg", Alt: img.Alt}, b.Dx(), b.Dy(), nil
			}
		}
		edge = edge * 3 / 4
	}
	return Image{}, 0, 0, errors.New("bsky: image does not fit the size limit")
}

// scaleDown returns src with its long edge at most edge, keeping the aspect ratio.
func scaleDown(src image.Image, edge int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= edge && h <= edge {
		return src
	}
	if w >= h {
		h = max(1, h*edge/w)
		w = edge
	} else {
		w = max(1, w*edge/h)
		h = edge
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}
