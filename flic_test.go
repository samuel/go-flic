package flic

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"image"
	"image/color"
	"image/gif"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"samuelks.com/gifopt"
)

var gifDir = flag.String("gifdir", "", "write a GIF of each test file rendered by TestDecoder to this directory")

func TestDecoder(t *testing.T) {
	files, err := filepath.Glob("testdata/*.fl?")
	if err != nil {
		t.Fatal(err)
	}
	// Files with known damage, which must be reported as an error.
	damaged := map[string]bool{"phasers.fli": true}
	for _, inPath := range files {
		t.Run(filepath.Base(inPath), func(t *testing.T) {
			wantErr := damaged[filepath.Base(inPath)]
			f, err := os.Open(inPath)
			if err != nil {
				t.Fatal(err)
			}
			//nolint:errcheck
			defer f.Close()

			d, err := NewDecoder(f, nil)
			if err != nil {
				t.Fatal(err)
			}
			for {
				c, err := d.ReadChunk()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					if !wantErr {
						t.Errorf("read chunk failed: %s", err)
					}
					if c == nil {
						break
					}
				}
				printChunk("", t.Logf, c)
				switch c := c.(type) {
				case *FrameChunk:
					for _, sc := range c.Chunks {
						if sc, ok := sc.(*UnknownChunk); ok {
							t.Errorf("Unknown subchunk type %04x", sc.Type)
						}
					}
				case *UnknownChunk:
					t.Errorf("Unknown chunk type %04x", c.Type)
				}
			}

			f, err = os.Open(inPath)
			if err != nil {
				t.Fatal(err)
			}
			//nolint:errcheck
			defer f.Close()
			g, err := RenderGIF(f, nil)
			switch {
			case err != nil && !wantErr:
				t.Fatal(err)
			case err == nil && wantErr:
				t.Fatal("expected an error for a damaged file")
			case g == nil:
				t.Fatalf("expected partial output, got none: %s", err)
			}

			if *gifDir == "" {
				return
			}
			outPath := filepath.Join(*gifDir, filepath.Base(inPath)+".gif")
			fo, err := os.Create(outPath)
			if err != nil {
				t.Fatalf("Failed to create output file %q: %s", outPath, err)
			}
			if err := gifopt.OptimizeGIF(fo, g, nil); err != nil {
				t.Fatalf("Failed to optimize GIF to output path %q: %s", outPath, err)
			}
			if err := fo.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func printChunks(inset string, p func(v string, a ...any), cs []Chunk) {
	for _, c := range cs {
		printChunk(inset, p, c)
	}
}

func printChunk(inset string, p func(v string, a ...any), c Chunk) {
	p(inset + c.String())
	if c, ok := c.(*ColorChunk); ok {
		for _, u := range c.Updates {
			p("%s\tUpdate offset=%d count=%d\n", inset, u.Offset, len(u.Colors))
		}
	}
	printChunks(inset+"\t", p, c.SubChunks())
}

// TestDecodeDeltaFLCLastPixelCurrentLine verifies that the last-pixel op in a
// DeltaFLC chunk sets the last pixel of the current line.
func TestDecodeDeltaFLCLastPixelCurrentLine(t *testing.T) {
	const (
		width  = 4
		height = 3
	)
	data := []byte{
		0x02, 0x00, // line count 2
		0x00, 0x00, // line 0: packet count 0
		0x42, 0x80, // line 1: last pixel = 0x42
		0x00, 0x00, // line 1: packet count 0
	}

	ops, err := decodeDeltaFLC(data, width, height)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(ops))
	}
	op := ops[0]
	if op.X != width-1 {
		t.Errorf("X: want %d, got %d", width-1, op.X)
	}
	if op.Y != 1 {
		t.Errorf("Y: want 1 (current line), got %d", op.Y)
	}
	if op.Count != 1 || len(op.Value) != 1 || op.Value[0] != 0x42 {
		t.Errorf("pixel value: want {Count:1, Value:[0x42]}, got {Count:%d, Value:%v}", op.Count, op.Value)
	}
}

// TestDecodeDeltaFLCLastPixelAtY0 verifies that a last-pixel op on the first line
// applies to that line rather than being dropped.
func TestDecodeDeltaFLCLastPixelAtY0(t *testing.T) {
	const (
		width  = 4
		height = 2
	)
	data := []byte{
		0x01, 0x00, // line count 1
		0x42, 0x80, // last pixel = 0x42
		0x00, 0x00, // packet count 0
	}

	ops, err := decodeDeltaFLC(data, width, height)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %d: %+v", len(ops), ops)
	}
	if ops[0].X != width-1 || ops[0].Y != 0 {
		t.Errorf("position: want (%d,0), got (%d,%d)", width-1, ops[0].X, ops[0].Y)
	}
}

// TestDecodeDeltaFLCLineSkipPastHeight verifies that a line skip that runs past
// the bottom of the image stops decoding instead of emitting out of range ops.
func TestDecodeDeltaFLCLineSkipPastHeight(t *testing.T) {
	const (
		width  = 4
		height = 2
	)
	data := []byte{
		0x01, 0x00, // line count 1
		0xfb, 0xff, // skip 5 lines, past the bottom of the image
		0x42, 0x80, // last pixel on line 5, out of range
		0x00, 0x00, // packet count 0
	}

	ops, err := decodeDeltaFLC(data, width, height)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 0 {
		t.Errorf("expected no ops past the image bottom, got %d: %+v", len(ops), ops)
	}
}

// TestDecodeByteRunClampsToWidth verifies that a packet longer than the scanline
// is clamped rather than spilling into the next line.
func TestDecodeByteRunClampsToWidth(t *testing.T) {
	const (
		width  = 4
		height = 1
	)
	// One packet count byte, then a run of 10 pixels of value 0x07 on a 4 pixel line.
	data := []byte{0x01, 0x0a, 0x07}

	ops, err := decodeByteRunData(data, width, height)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(ops))
	}
	if ops[0].Count != width {
		t.Errorf("Count: want %d, got %d", width, ops[0].Count)
	}
}

// TestApplyDrawOpsClipping verifies that ops outside the image are dropped and
// that writes never cross into the following scanline.
func TestApplyDrawOpsClipping(t *testing.T) {
	const (
		w = 4
		h = 2
	)
	im := image.NewPaletted(image.Rect(0, 0, w, h), make([]color.Color, 256))
	ops := []DrawOp{
		// Overruns the end of line 0; must not touch line 1.
		{X: 2, Y: 0, Count: 8, Value: []byte{1}},
		// Entirely outside the image; must be dropped rather than panic.
		{X: 0, Y: 99, Count: 1, Value: []byte{2}},
		{X: 99, Y: 0, Count: 1, Value: []byte{3}},
		{X: -1, Y: 0, Count: 1, Value: []byte{4}},
	}

	ApplyDrawOps(im, ops)

	want := []byte{0, 0, 1, 1, 0, 0, 0, 0}
	for i, v := range want {
		if im.Pix[i] != v {
			t.Errorf("pix[%d]: want %d, got %d (all: %v)", i, v, im.Pix[i], im.Pix)
			break
		}
	}
}

// TestColorChunkScaling verifies that Color64 components are expanded to the
// full 8 bit range while Color256 components are used as is.
func TestColorChunkScaling(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  ChunkType
		want color.NRGBA
	}{
		{"Color64", ChunkColor64, color.NRGBA{R: 255, G: 0, B: 129, A: 255}},
		{"Color256", ChunkColor256, color.NRGBA{R: 63, G: 0, B: 32, A: 255}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Decoder{buf: make([]byte, 1024)}
			// One update: skip 0, copy 1 color (63, 0, 32).
			data := []byte{0x01, 0x00, 0x00, 0x01, 63, 0, 32}
			c, err := d.readColorChunk(bytes.NewReader(data), tc.typ)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Updates) != 1 || len(c.Updates[0].Colors) != 1 {
				t.Fatalf("unexpected updates: %+v", c.Updates)
			}
			if got := c.Updates[0].Colors[0]; got != tc.want {
				t.Errorf("color: want %+v, got %+v", tc.want, got)
			}
		})
	}
}

// TestApplyBlackChunk verifies that ApplyBlackChunk zeroes all pixels.
func TestApplyBlackChunk(t *testing.T) {
	pal := make([]color.Color, 256)
	for i := range pal {
		pal[i] = color.NRGBA{R: uint8(i), A: 255}
	}
	im := image.NewPaletted(image.Rect(0, 0, 4, 4), pal)
	for i := range im.Pix {
		im.Pix[i] = uint8(i + 1)
	}

	ApplyBlackChunk(im)

	for i, p := range im.Pix {
		if p != 0 {
			t.Errorf("pixel[%d] = %d after ApplyBlackChunk, want 0", i, p)
		}
	}
}

// TestRenderPostageStampFLICopy verifies that RenderPostageStamp handles an
// uncompressed FLICopyChunk as well as a ByteRun DrawChunk.
func TestRenderPostageStampFLICopy(t *testing.T) {
	const (
		w = 3
		h = 2
	)
	bitmap := []byte{0, 1, 2, 3, 4, 5}

	psc := &PostageStampChunk{
		Width:  w,
		Height: h,
		Chunk:  &FLICopyChunk{Width: w, Height: h, Bitmap: bitmap},
	}

	im, err := RenderPostageStamp(psc)
	if err != nil {
		t.Fatal(err)
	}
	if got := im.Bounds(); got != image.Rect(0, 0, w, h) {
		t.Errorf("bounds: want %v, got %v", image.Rect(0, 0, w, h), got)
	}
	for i, want := range bitmap {
		if got := im.Pix[i]; got != want {
			t.Errorf("pix[%d]: want %d, got %d", i, want, got)
		}
	}
}

// TestApplyFLICopyChunk verifies that an uncompressed full-frame bitmap is
// copied into the image.
func TestApplyFLICopyChunk(t *testing.T) {
	const (
		w = 3
		h = 2
	)
	im := image.NewPaletted(image.Rect(0, 0, w, h), make([]color.Color, 256))
	bitmap := []byte{5, 4, 3, 2, 1, 0}

	ApplyFLICopyChunk(im, &FLICopyChunk{Width: w, Height: h, Bitmap: bitmap})

	for i, want := range bitmap {
		if got := im.Pix[i]; got != want {
			t.Errorf("pix[%d]: want %d, got %d", i, want, got)
		}
	}
}

// TestApplyFLICopyChunkStride verifies that the bitmap is copied row by row
// into an image wider than the bitmap.
func TestApplyFLICopyChunkStride(t *testing.T) {
	im := image.NewPaletted(image.Rect(0, 0, 4, 2), make([]color.Color, 256))
	ApplyFLICopyChunk(im, &FLICopyChunk{Width: 3, Height: 2, Bitmap: []byte{1, 2, 3, 4, 5, 6}})
	want := []byte{1, 2, 3, 0, 4, 5, 6, 0}
	if !bytes.Equal(im.Pix, want) {
		t.Errorf("want %v, got %v", want, im.Pix)
	}
}

// TestApplyBlackChunkSubImage verifies that only the pixels of a sub-image are
// cleared.
func TestApplyBlackChunkSubImage(t *testing.T) {
	im := image.NewPaletted(image.Rect(0, 0, 3, 2), make([]color.Color, 256))
	for i := range im.Pix {
		im.Pix[i] = 1
	}
	ApplyBlackChunk(im.SubImage(image.Rect(1, 0, 3, 1)).(*image.Paletted))
	want := []byte{1, 0, 0, 1, 1, 1}
	if !bytes.Equal(im.Pix, want) {
		t.Errorf("want %v, got %v", want, im.Pix)
	}
}

// TestApplyColorChunkShortPalette verifies that a palette shorter than an
// update is extended rather than causing a panic.
func TestApplyColorChunkShortPalette(t *testing.T) {
	im := image.NewPaletted(image.Rect(0, 0, 1, 1), make([]color.Color, 2))
	red := color.NRGBA{R: 255, A: 255}
	ApplyColorChunk(im, &ColorChunk{Updates: []PaletteUpdate{{Offset: 4, Colors: []color.NRGBA{red}}}})
	if len(im.Palette) != 5 {
		t.Fatalf("palette length: want 5, got %d", len(im.Palette))
	}
	if im.Palette[2] != (color.NRGBA{A: 255}) || im.Palette[4] != red {
		t.Errorf("unexpected palette %v", im.Palette)
	}
}

func TestChunkTypeString(t *testing.T) {
	if got := ChunkByteRun.String(); got != "ByteRun" {
		t.Errorf("want ByteRun, got %s", got)
	}
	if got := ChunkType(0x99).String(); got != "ChunkType(0x0099)" {
		t.Errorf("want ChunkType(0x0099), got %s", got)
	}
}

// TestOddChunkSizes verifies that chunks with odd sizes are not followed by a
// pad byte, for both top-level frames and the subchunks inside them.
func TestOddChunkSizes(t *testing.T) {
	var buf bytes.Buffer
	write := func(v any) {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	// A Color256 chunk with one update of one color: 6 byte header plus 7 byte body.
	color256 := func(skip, r, g, b byte) {
		write(chunkHeader{Size: 13, Type: ChunkColor256})
		write([]byte{1, 0, skip, 1, r, g, b})
	}
	frame := func(size uint32, subChunks uint16) {
		write(chunkHeader{Size: size, Type: ChunkFrame})
		write(FrameChunkHeader{SubChunks: subChunks})
	}
	frame(16+13+13, 2)
	color256(0, 1, 2, 3)
	color256(1, 4, 5, 6)
	frame(16+13, 1) // odd-sized frame
	color256(2, 7, 8, 9)
	frame(16, 0)

	d, err := NewDecoder(withHeader(t, buf.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	var frames []*FrameChunk
	for c, err := range d.Chunks() {
		if err != nil {
			t.Fatal(err)
		}
		fc, ok := c.(*FrameChunk)
		if !ok {
			t.Fatalf("expected *FrameChunk, got %T", c)
		}
		frames = append(frames, fc)
	}
	if len(frames) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(frames))
	}
	want := []struct {
		frame, chunk, offset int
		color                color.NRGBA
	}{
		{0, 0, 0, color.NRGBA{R: 1, G: 2, B: 3, A: 255}},
		{0, 1, 1, color.NRGBA{R: 4, G: 5, B: 6, A: 255}},
		{1, 0, 2, color.NRGBA{R: 7, G: 8, B: 9, A: 255}},
	}
	for _, w := range want {
		cc, ok := frames[w.frame].Chunks[w.chunk].(*ColorChunk)
		if !ok {
			t.Fatalf("frame %d chunk %d: expected *ColorChunk, got %T", w.frame, w.chunk, frames[w.frame].Chunks[w.chunk])
		}
		u := cc.Updates[0]
		if u.Offset != w.offset || u.Colors[0] != w.color {
			t.Errorf("frame %d chunk %d: want offset %d color %+v, got offset %d color %+v", w.frame, w.chunk, w.offset, w.color, u.Offset, u.Colors[0])
		}
	}
}

// withHeader returns an FLC file with a 1x1 header followed by body.
func withHeader(t *testing.T, body []byte) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	h := &Header{Size: uint32(headerSize + len(body)), Type: FileTypeFLC, Width: 1, Height: 1}
	if err := binary.Write(&buf, binary.LittleEndian, h); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != headerSize {
		t.Fatalf("header is %d bytes, expected %d", buf.Len(), headerSize)
	}
	buf.Write(body)
	return &buf
}

// TestResync verifies that junk between frames is reported as an error by
// default, and is skipped and reported as a *SkippedDataError with Resync.
func TestResync(t *testing.T) {
	var body bytes.Buffer
	write := func(v any) {
		if err := binary.Write(&body, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	write(chunkHeader{Size: 16, Type: ChunkFrame})
	write(FrameChunkHeader{})
	junkStart := int64(headerSize + body.Len())
	// Reads as a chunk of unknown type running far past the end of the file.
	write(chunkHeader{Size: 1 << 20, Type: 0x1234})
	// Frame and segment table headers that must not be taken as resync points:
	// the frame's reserved field is not zero, and a segment table is never
	// accepted.
	write(chunkHeader{Size: 16, Type: ChunkFrame})
	write(FrameChunkHeader{Reserved: 1})
	write(chunkHeader{Size: 1 << 20, Type: ChunkSegmentTable})
	write([]byte{1, 2, 3})
	junkLen := int64(headerSize+body.Len()) - junkStart
	write(chunkHeader{Size: 16 + 13, Type: ChunkFrame})
	write(FrameChunkHeader{SubChunks: 1})
	write(chunkHeader{Size: 13, Type: ChunkColor256})
	write([]byte{1, 0, 0, 1, 7, 8, 9})

	t.Run("Default", func(t *testing.T) {
		f, err := Decode(withHeader(t, body.Bytes()), nil)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
		}
		if len(f.Chunks) != 1 {
			t.Errorf("expected 1 chunk before the junk, got %d", len(f.Chunks))
		}
	})
	t.Run("Resync", func(t *testing.T) {
		f, err := Decode(withHeader(t, body.Bytes()), &Options{Resync: true})
		var skipped *SkippedDataError
		if !errors.As(err, &skipped) {
			t.Fatalf("expected *SkippedDataError, got %v", err)
		}
		if skipped.Offset != junkStart || skipped.Length != junkLen {
			t.Errorf("want offset %d length %d, got offset %d length %d", junkStart, junkLen, skipped.Offset, skipped.Length)
		}
		if len(f.Chunks) != 2 {
			t.Fatalf("expected 2 chunks, got %d", len(f.Chunks))
		}
		fc, ok := f.Chunks[1].(*FrameChunk)
		if !ok || len(fc.Chunks) != 1 {
			t.Fatalf("expected a frame with 1 subchunk after the junk, got %v", f.Chunks[1])
		}
	})
}

// TestResyncPhasers verifies resync on a real damaged file, which has a 64 KB
// block of non-FLIC data after its 60th frame and a truncated last frame.
func TestResyncPhasers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resync     bool
		wantFrames int
	}{
		{"Default", false, 60},
		// 60 frames before the block, 38 after, and the truncated last frame.
		{"Resync", true, 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Open("testdata/phasers.fli")
			if err != nil {
				t.Fatal(err)
			}
			//nolint:errcheck
			defer f.Close()
			g, err := RenderGIF(f, &Options{Resync: tc.resync})
			if g == nil {
				t.Fatalf("expected partial output, got none: %s", err)
			}
			if len(g.Image) != tc.wantFrames {
				t.Errorf("expected %d frames, got %d", tc.wantFrames, len(g.Image))
			}
			var skipped *SkippedDataError
			if tc.resync {
				if !errors.As(err, &skipped) {
					t.Fatalf("expected *SkippedDataError, got %v", err)
				}
				if skipped.Offset != 16484 || skipped.Length != 65536 {
					t.Errorf("want offset 16484 length 65536, got offset %d length %d", skipped.Offset, skipped.Length)
				}
			} else {
				if errors.As(err, &skipped) {
					t.Errorf("unexpected *SkippedDataError without resync: %v", err)
				}
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
				}
			}
		})
	}
}

// TestRenderGIFFrames verifies frame counts and delays of the rendered test files.
// ship.flc ends with a ring frame, which is dropped. alex.flc has one more frame
// than its header says, but it isn't a ring frame so it's kept.
func TestRenderGIFFrames(t *testing.T) {
	for _, tc := range []struct {
		file       string
		wantFrames int
		wantDelay  int
	}{
		{"alex.flc", 3, 7},     // 71 ms
		{"ship.flc", 2, 7},     // 71 ms
		{"phasers.fli", 60, 3}, // 2/70 s, about 28.6 ms
	} {
		t.Run(tc.file, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			//nolint:errcheck
			defer f.Close()
			g, _ := RenderGIF(f, nil)
			if g == nil {
				t.Fatal("expected output")
			}
			if len(g.Image) != tc.wantFrames {
				t.Errorf("expected %d frames, got %d", tc.wantFrames, len(g.Image))
			}
			for i, d := range g.Delay {
				if d != tc.wantDelay {
					t.Errorf("frame %d: expected delay %d, got %d", i, tc.wantDelay, d)
					break
				}
			}
		})
	}
}

// TestDropRingFrame verifies that the last frame is dropped only when the frame
// count is one more than the header says and it matches the first frame.
func TestDropRingFrame(t *testing.T) {
	frame := func(v byte, c color.Color) *image.Paletted {
		im := image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{c})
		im.Pix[0] = v
		return im
	}
	black, white := color.NRGBA{A: 255}, color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	for _, tc := range []struct {
		name   string
		images []*image.Paletted
		frames int
		want   int
	}{
		{"RingFrame", []*image.Paletted{frame(0, black), frame(1, black), frame(0, black)}, 2, 2},
		{"DifferentPixels", []*image.Paletted{frame(0, black), frame(1, black), frame(1, black)}, 2, 3},
		{"DifferentPalette", []*image.Paletted{frame(0, black), frame(1, black), frame(0, white)}, 2, 3},
		{"CountMatchesHeader", []*image.Paletted{frame(0, black), frame(1, black), frame(0, black)}, 3, 3},
		{"NoHeaderCount", []*image.Paletted{frame(0, black)}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &gif.GIF{Image: tc.images, Delay: make([]int, len(tc.images))}
			dropRingFrame(g, tc.frames)
			if len(g.Image) != tc.want || len(g.Delay) != tc.want {
				t.Errorf("expected %d frames and delays, got %d and %d", tc.want, len(g.Image), len(g.Delay))
			}
		})
	}
}

func TestDecodeByteRunData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		width   int
		height  int
		data    []byte
		want    []DrawOp
		wantErr bool
	}{
		{
			name:  "Literal",
			width: 4, height: 1,
			data: []byte{1, 0xfc, 1, 2, 3, 4}, // literal run of 4
			want: []DrawOp{{X: 0, Y: 0, Count: 1, Value: []byte{1, 2, 3, 4}}},
		},
		{
			name:  "ZeroLengthEndsLine",
			width: 4, height: 2,
			data: []byte{
				1, 0x00, // line 0: zero-length packet
				1, 0x04, 9, // line 1: run of 4
			},
			want: []DrawOp{{X: 0, Y: 1, Count: 4, Value: []byte{9}}},
		},
		{
			name:  "PrematureEnd",
			width: 4, height: 1,
			data:    []byte{1, 0xfc, 1, 2},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops, err := decodeByteRunData(tc.data, tc.width, tc.height)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !equalOps(ops, tc.want) {
				t.Errorf("want %+v, got %+v", tc.want, ops)
			}
		})
	}
}

func TestDecodeDeltaFLI(t *testing.T) {
	const (
		width  = 8
		height = 4
	)
	data := []byte{
		1, 0, // start at line 1
		3, 0, // 3 lines
		// Line 1
		2,          // 2 packets
		2, 2, 5, 6, // skip 2, literal [5 6]
		1, 0xfd, 7, // skip 1, run of 3 x 7
		// Line 2
		1,                // 1 packet
		6, 4, 1, 2, 3, 4, // skip 6, literal of 4 clipped to 2
		// Line 3
		1,       // 1 packet
		9, 1, 1, // skip 9, past the end of the line
	}
	ops, err := decodeDeltaFLI(data, width, height)
	if err != nil {
		t.Fatal(err)
	}
	want := []DrawOp{
		{X: 2, Y: 1, Count: 1, Value: []byte{5, 6}},
		{X: 5, Y: 1, Count: 3, Value: []byte{7}},
		{X: 6, Y: 2, Count: 1, Value: []byte{1, 2}},
	}
	if !equalOps(ops, want) {
		t.Errorf("want %+v, got %+v", want, ops)
	}
}

func equalOps(a, b []DrawOp) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].X != b[i].X || a[i].Y != b[i].Y || a[i].Count != b[i].Count || !bytes.Equal(a[i].Value, b[i].Value) {
			return false
		}
	}
	return true
}

func TestColorChunkErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"PaletteOverrun", []byte{1, 0, 200, 100}}, // skip 200, copy 100
		{"TooManyUpdates", []byte{0x01, 0x01}},     // 257 updates
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Decoder{buf: make([]byte, 1024)}
			if _, err := d.readColorChunk(bytes.NewReader(tc.data), ChunkColor256); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestFrameDelay(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    Header
		want time.Duration
	}{
		{"FLI", Header{Type: FileTypeFLI, Speed: 7}, 100 * time.Millisecond}, // 7/70 s
		{"FLC", Header{Type: FileTypeFLC, Speed: 71}, 71 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.h.FrameDelay(); got != tc.want {
				t.Errorf("want %d, got %d", tc.want, got)
			}
		})
	}
}

func TestRenderGIFNoFrames(t *testing.T) {
	g, err := RenderGIF(withHeader(t, nil), nil)
	if !errors.Is(err, ErrNoFrames) {
		t.Errorf("expected ErrNoFrames, got %v", err)
	}
	if g != nil {
		t.Errorf("expected no output, got %d frames", len(g.Image))
	}
}

// TestRenderGIFMaxPixels verifies that rendering stops once the frames would
// exceed Options.MaxPixels. ship.flc has 640x480 frames.
func TestRenderGIFMaxPixels(t *testing.T) {
	const frame = 640 * 480
	for _, tc := range []struct {
		name       string
		maxPixels  int
		wantFrames int
		wantErr    bool
	}{
		{"NoLimit", 0, 2, false},
		{"SmallerThanFrame", frame - 1, 0, true},
		{"OneFrame", frame, 1, true},
		{"TwoFramesAndPart", 3*frame - 1, 2, true},
		{"Exact", 3 * frame, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Open("testdata/ship.flc")
			if err != nil {
				t.Fatal(err)
			}
			//nolint:errcheck
			defer f.Close()
			g, err := RenderGIF(f, &Options{MaxPixels: tc.maxPixels})
			if got := errors.Is(err, ErrPixelLimit); got != tc.wantErr {
				t.Fatalf("expected ErrPixelLimit %v, got %v", tc.wantErr, err)
			}
			var frames int
			if g != nil {
				frames = len(g.Image)
			}
			if frames != tc.wantFrames {
				t.Errorf("expected %d frames, got %d", tc.wantFrames, frames)
			}
		})
	}
}

// TestPostageStampTruncatedUnknown verifies that an unknown postage stamp
// subchunk claiming a huge size is reported as truncated.
func TestPostageStampTruncatedUnknown(t *testing.T) {
	var body bytes.Buffer
	write := func(v any) {
		if err := binary.Write(&body, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	write(chunkHeader{Size: 16 + 18, Type: ChunkFrame})
	write(FrameChunkHeader{SubChunks: 1})
	write(chunkHeader{Size: 18, Type: ChunkPostageStamp})
	write([3]uint16{1, 1, 0}) // height, width, xlate
	write(chunkHeader{Size: 0xfffffff0, Type: 0x99})

	_, err := Decode(withHeader(t, body.Bytes()), nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
	}
}
