package flic

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"io"
	"slices"
	"time"
)

var (
	// ErrNoFrames is returned by RenderGIF for a file with no frames.
	ErrNoFrames = errors.New("flic: no frames rendered")
	// ErrPixelLimit is wrapped by the error RenderGIF returns when the frames
	// would exceed Options.MaxPixels.
	ErrPixelLimit = errors.New("flic: pixel limit exceeded")
)

// RenderGIF renders each frame of a FLIC as a full image. The result can be encoded
// with gif.EncodeAll, but full frames make a large file, so running it through a GIF
// optimizer first is worthwhile. Unknown chunks are ignored. On a read error, the
// frames rendered so far are returned along with the error. Once the frames would
// exceed Options.MaxPixels, rendering stops with an error wrapping ErrPixelLimit.
func RenderGIF(r io.Reader, opts *Options) (*gif.GIF, error) {
	d, err := NewDecoder(r, opts)
	if err != nil {
		return nil, err
	}

	frameDelay := d.Header.FrameDelay()
	var maxPixels int
	if opts != nil {
		maxPixels = opts.MaxPixels
	}
	framePixels := int(d.Header.Width) * int(d.Header.Height)
	if maxPixels > 0 && framePixels > maxPixels {
		return nil, fmt.Errorf("%w: a %dx%d frame is more than %d pixels",
			ErrPixelLimit, d.Header.Width, d.Header.Height, maxPixels)
	}

	g := &gif.GIF{
		LoopCount: 0,
	}

	var errs []error
	pal := make([]color.Color, 256)
	for i := range pal {
		pal[i] = color.NRGBA{A: 255}
	}
	im := image.NewPaletted(image.Rect(0, 0, int(d.Header.Width), int(d.Header.Height)), pal)
loop:
	for c, err := range d.Chunks() {
		if err != nil {
			errs = append(errs, err)
			if c == nil {
				break
			}
		}
		switch c := c.(type) {
		case *FrameChunk:
			if maxPixels > 0 && (len(g.Image)+1)*framePixels > maxPixels {
				errs = append(errs, fmt.Errorf("%w: frame %d would bring the total over %d pixels",
					ErrPixelLimit, len(g.Image)+1, maxPixels))
				break loop
			}
			for _, sc := range c.Chunks {
				switch sc := sc.(type) {
				case *ColorChunk:
					ApplyColorChunk(im, sc)
				case *DrawChunk:
					ApplyDrawChunk(im, sc)
				case *FLICopyChunk:
					ApplyFLICopyChunk(im, sc)
				case *BlackChunk:
					ApplyBlackChunk(im)
				}
			}
			im2 := image.NewPaletted(im.Bounds(), append([]color.Color(nil), im.Palette...))
			copy(im2.Pix, im.Pix)
			g.Image = append(g.Image, im2)
			// The per-frame delay override is ignored because it doesn't look reliable.
			// GIF delays are in centiseconds.
			g.Delay = append(g.Delay, int((frameDelay+5*time.Millisecond)/(10*time.Millisecond)))
		}
	}
	dropRingFrame(g, int(d.Header.Frames))
	if len(errs) != 0 {
		if len(g.Image) == 0 {
			g = nil
		}
		return g, errors.Join(errs...)
	}
	if len(g.Image) == 0 {
		return nil, ErrNoFrames
	}
	return g, nil
}

// dropRingFrame removes the ring frame, the extra last frame that returns the
// animation to its first frame. It must match the first frame, since a file with a
// wrong header frame count can have a real frame in that position.
func dropRingFrame(g *gif.GIF, frames int) {
	if frames < 1 || len(g.Image) != frames+1 {
		return
	}
	first, last := g.Image[0], g.Image[frames]
	if !slices.Equal(first.Pix, last.Pix) || !slices.Equal(first.Palette, last.Palette) {
		return
	}
	g.Image = g.Image[:frames]
	g.Delay = g.Delay[:frames]
}

// RenderPostageStamp returns a rendered version of a postage stamp chunk.
func RenderPostageStamp(c *PostageStampChunk) (*image.Paletted, error) {
	// Paletted pixels are 8-bit indices but the six-cube palette only covers 216
	// entries, so pad it to keep any out-of-cube index in range.
	pal := make(color.Palette, 256)
	n := copy(pal, PostageStampPalette())
	for i := n; i < len(pal); i++ {
		pal[i] = color.NRGBA{A: 255}
	}
	im := image.NewPaletted(image.Rect(0, 0, int(c.Width), int(c.Height)), pal)
	switch sc := c.Chunk.(type) {
	case *DrawChunk:
		ApplyDrawOps(im, sc.Ops)
	case *FLICopyChunk:
		ApplyFLICopyChunk(im, sc)
	case nil:
		return nil, errors.New("flic: postage stamp has no subchunk")
	default:
		return nil, fmt.Errorf("flic: unhandled postage stamp subchunk %v", sc.ChunkType())
	}
	return im, nil
}

// ApplyDrawOps applies a set of draw operations to an image. Operations that fall
// outside the image are dropped and writes are clipped to the end of their scanline.
func ApplyDrawOps(im *image.Paletted, ops []DrawOp) {
	b := im.Bounds()
	width, height := b.Dx(), b.Dy()
	for _, o := range ops {
		if o.X < 0 || o.X >= width || o.Y < 0 || o.Y >= height || len(o.Value) == 0 {
			continue
		}
		off := o.Y*im.Stride + o.X
		rowEnd := o.Y*im.Stride + width
		for range o.Count {
			if off >= rowEnd {
				break
			}
			off += copy(im.Pix[off:rowEnd], o.Value)
		}
	}
}

// ApplyDrawChunk applies the ops from a DrawChunk to im.
func ApplyDrawChunk(im *image.Paletted, c *DrawChunk) {
	ApplyDrawOps(im, c.Ops)
}

// ApplyFLICopyChunk copies the bitmap from a FLICopyChunk into the top-left of
// im. The parts of the bitmap that fall outside im are dropped.
func ApplyFLICopyChunk(im *image.Paletted, c *FLICopyChunk) {
	b := im.Bounds()
	width := min(c.Width, b.Dx())
	for y := range min(c.Height, b.Dy()) {
		src := c.Bitmap[min(y*c.Width, len(c.Bitmap)):]
		copy(im.Pix[y*im.Stride:y*im.Stride+width], src)
	}
}

// ApplyBlackChunk sets all pixels in the image to palette index 0.
func ApplyBlackChunk(im *image.Paletted) {
	width := im.Bounds().Dx()
	for y := range im.Bounds().Dy() {
		clear(im.Pix[y*im.Stride : y*im.Stride+width])
	}
}

// ApplyColorChunk applies the palette updates from a ColorChunk to im. If the
// palette is too short for an update, it is extended with opaque black.
func ApplyColorChunk(im *image.Paletted, c *ColorChunk) {
	for _, u := range c.Updates {
		for len(im.Palette) < u.Offset+len(u.Colors) {
			im.Palette = append(im.Palette, color.NRGBA{A: 255})
		}
		for i, c := range u.Colors {
			im.Palette[u.Offset+i] = c
		}
	}
}
