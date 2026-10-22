// Package flic decodes FLIC animations (.fli and .flc files from Autodesk Animator
// and Animator Pro) and renders them to animated GIF.
package flic

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"image/color"
	"io"
	"iter"
	"time"
)

// MediaType is the MIME type of FLIC files.
const MediaType = "video/x-flic"

// File types, as stored in Header.Type. Only FileTypeFLI and FileTypeFLC are
// decoded.
const (
	FileTypeFLI  = 0xAF11 // FLI
	FileTypeFLC  = 0xAF12 // FLC with 8-bit color. Tempra FLX and Autodesk FLX files use it too.
	FileTypeAF44 = 0xAF44 // Color depth other than 8 bits (1, 15, 16, or 24), e.g. from DTA or EGI 3.0+.
	FileTypeAF30 = 0xAF30 // Huffman or BWT compression, possibly with a color depth other than 8 bits.
	FileTypeAF31 = 0xAF31 // "Frame shift" compression, possibly with Huffman, BWT, or a color depth other than 8 bits.
	// Password-protected files scramble the type field so that other readers reject them.
)

// Header is the 128-byte header at the start of a FLIC file.
type Header struct {
	Size         uint32   // Size of FLIC including this header
	Type         uint16   // File type 0xAF11, 0xAF12, 0xAF30, 0xAF44, ...
	Frames       uint16   // Number of frames in first segment
	Width        uint16   // FLIC width in pixels
	Height       uint16   // FLIC height in pixels
	Depth        uint16   // Bits per pixel (usually 8)
	Flags        uint16   // Set to zero or to three
	Speed        uint32   // Delay between frames: 1/70 s units for FLI, milliseconds for FLC
	Reserved1    uint16   // Set to zero
	Created      uint32   // Date of FLIC creation (FLC only)
	Creator      uint32   // Serial number or compiler id (FLC only)
	Updated      uint32   // Date of FLIC update (FLC only)
	Updater      uint32   // Serial number (FLC only), see creator
	AspectDx     uint16   // Width of square rectangle (FLC only)
	AspectDy     uint16   // Height of square rectangle (FLC only)
	ExtFlags     uint16   // EGI: flags for specific EGI extensions
	KeyFrames    uint16   // EGI: key-image frequency
	TotalFrames  uint16   // EGI: total number of frames (segments)
	ReqMemory    uint32   // EGI: maximum chunk size (uncompressed)
	MaxRegions   uint16   // EGI: max. number of regions in a CHK_REGION chunk
	TranspNum    uint16   // EGI: number of transparent levels
	Reserved2    [24]byte // Set to zero
	Frame1Offset uint32   // Offset to frame 1 (FLC only)
	Frame2Offset uint32   // Offset to frame 2 (FLC only)
	Reserved3    [40]byte // Set to zero
}

// FrameDelay returns the delay between frames.
func (h *Header) FrameDelay() time.Duration {
	if h.Type == FileTypeFLI {
		return time.Duration(h.Speed) * time.Second / 70
	}
	return time.Duration(h.Speed) * time.Millisecond
}

// File is a decoded FLIC file.
type File struct {
	Header *Header
	Chunks []Chunk
}

// ChunkType identifies the kind of a chunk.
type ChunkType uint16

// Chunk types. Only some are decoded into their own Chunk types; the rest come
// back as *UnknownChunk.
const (
	ChunkCelData        ChunkType = 3  // Registration and transparency
	ChunkColor256       ChunkType = 4  // 256-level color palette
	ChunkDeltaFLC       ChunkType = 7  // (FLI_SS2) Delta image, word oriented RLE
	ChunkColor64        ChunkType = 11 // 64-level color palette
	ChunkDeltaFLI       ChunkType = 12 // (FLI_LC) Delta image, byte oriented RLE
	ChunkBlack          ChunkType = 13 // Full black frame (rare)
	ChunkByteRun        ChunkType = 15 // (FLI_BRUN) Full image, byte oriented RLE
	ChunkFLICopy        ChunkType = 16 // Uncompressed image (rare)
	ChunkPostageStamp   ChunkType = 18 // Postage stamp (icon of the first frame)
	ChunkDTAByteRun     ChunkType = 25 // Full image, pixel oriented RLE
	ChunkDTACopy        ChunkType = 26 // Uncompressed image
	ChunkDTADelta       ChunkType = 27 // Delta image, pixel oriented RLE
	ChunkLabel          ChunkType = 31 // Frame label
	ChunkBitmapMask     ChunkType = 32 // Bitmap mask
	ChunkMultilevelMask ChunkType = 33 // Multilevel mask
	ChunkSegment        ChunkType = 34 // Segment information
	ChunkKeyImage       ChunkType = 35 // Key image, similar to ByteRun / DTAByteRun
	ChunkKeyPalette     ChunkType = 36 // Key palette, similar to Color256
	ChunkRegion         ChunkType = 37 // Region of frame differences
	ChunkWave           ChunkType = 38 // Digitized audio
	ChunkUserString     ChunkType = 39 // General purpose user data
	ChunkRegionMask     ChunkType = 40 // Region mask
	ChunkLabelEx        ChunkType = 41 // Extended frame label (includes symbolic name)
	ChunkShift          ChunkType = 42 // Scanline delta shifts (compression)
	ChunkPathMap        ChunkType = 43 // Path map (segment transitions)

	ChunkPrefix       ChunkType = 0xF100 // Prefix chunk
	ChunkScript       ChunkType = 0xF1E0 // Embedded "Small" script
	ChunkFrame        ChunkType = 0xF1FA // Frame chunk
	ChunkSegmentTable ChunkType = 0xF1FB // Segment table chunk
	ChunkHuffmanTable ChunkType = 0xF1FC // Huffman compression table chunk
)

var chunkTypeName = map[ChunkType]string{
	ChunkCelData:        "CelData",
	ChunkColor256:       "Color256",
	ChunkDeltaFLC:       "DeltaFLC",
	ChunkColor64:        "Color64",
	ChunkDeltaFLI:       "DeltaFLI",
	ChunkBlack:          "Black",
	ChunkByteRun:        "ByteRun",
	ChunkFLICopy:        "FLICopy",
	ChunkPostageStamp:   "PostageStamp",
	ChunkDTAByteRun:     "DTAByteRun",
	ChunkDTACopy:        "DTACopy",
	ChunkDTADelta:       "DTADelta",
	ChunkLabel:          "Label",
	ChunkBitmapMask:     "BitmapMask",
	ChunkMultilevelMask: "MultilevelMask",
	ChunkSegment:        "Segment",
	ChunkKeyImage:       "KeyImage",
	ChunkKeyPalette:     "KeyPalette",
	ChunkRegion:         "Region",
	ChunkWave:           "Wave",
	ChunkUserString:     "UserString",
	ChunkRegionMask:     "RegionMask",
	ChunkLabelEx:        "LabelEx",
	ChunkShift:          "Shift",
	ChunkPathMap:        "PathMap",

	ChunkPrefix:       "Prefix",
	ChunkScript:       "Script",
	ChunkFrame:        "Frame",
	ChunkSegmentTable: "SegmentTable",
	ChunkHuffmanTable: "HuffmanTable",
}

func (t ChunkType) String() string {
	if name, ok := chunkTypeName[t]; ok {
		return name
	}
	return fmt.Sprintf("ChunkType(0x%04x)", uint16(t))
}

// PostageStampPalette returns the 216 color 6x6x6 cube that postage stamp
// images use.
func PostageStampPalette() color.Palette {
	pal := make(color.Palette, 0, 6*6*6)
	for r := range 6 {
		for g := range 6 {
			for b := range 6 {
				// https://github.com/AnimatorPro/Animator-Pro/blob/6d0b68cd94bb5cfde2cdd05e9a7c8ee1e1cb3cbb/src/PJ/poco/poekit/pstamp/pstamp.c#L422
				pal = append(pal, color.NRGBA{
					R: byte(256 * r / 6),
					G: byte(256 * g / 6),
					B: byte(256 * b / 6),
					A: 255,
				})
			}
		}
	}
	return pal
}

type chunkHeader struct {
	// Size of the chunk including this header. The spec says writers should round
	// it up to an even number, but some files store odd sizes with no pad byte, so
	// the next chunk always starts exactly Size bytes after this one.
	Size uint32
	Type ChunkType
}

// Options controls decoding and rendering. A nil *Options uses the defaults.
type Options struct {
	// Resync skips data that doesn't look like a chunk and resumes at the next
	// plausible chunk instead of stopping. Each skip is reported as a
	// *SkippedDataError along with the chunk that follows it. Frames after a skip
	// may render incorrectly because their deltas apply to the last frame before
	// the damage, not to whatever frames the damage replaced.
	Resync bool

	// MaxPixels limits the total number of pixels in the frames RenderGIF
	// returns. Every frame is a full image, so a small file with large dimensions
	// can otherwise need gigabytes of memory. Zero means no limit. Decode and the
	// Decoder ignore it.
	MaxPixels int
}

// SkippedDataError reports data that was skipped while resyncing after damage.
type SkippedDataError struct {
	Offset int64 // File offset where the skipped data starts
	Length int64 // Number of bytes skipped
}

func (e *SkippedDataError) Error() string {
	return fmt.Sprintf("flic: skipped %d bytes of invalid data at offset %d", e.Length, e.Offset)
}

// Decoder reads chunks from a FLIC file one at a time.
type Decoder struct {
	Header *Header
	cr     *countingReader
	r      *bufio.Reader
	buf    []byte
	resync bool
}

// NewDecoder reads the file header from r and returns a decoder for the chunks
// that follow. It returns an error for unsupported file types and zero dimensions.
func NewDecoder(r io.Reader, opts *Options) (*Decoder, error) {
	cr := &countingReader{r: r}
	d := &Decoder{
		Header: &Header{},
		buf:    make([]byte, 1024),
		cr:     cr,
		r:      bufio.NewReader(cr),
	}
	if opts != nil {
		d.resync = opts.Resync
	}
	if err := binary.Read(d.r, binary.LittleEndian, d.Header); err != nil {
		return nil, fmt.Errorf("flic: failed to read header: %w", err)
	}
	switch d.Header.Type {
	case FileTypeFLI, FileTypeFLC:
	default:
		return nil, fmt.Errorf("flic: unknown file type %04x", d.Header.Type)
	}
	if d.Header.Width == 0 || d.Header.Height == 0 {
		return nil, fmt.Errorf("flic: invalid dimensions %dx%d", d.Header.Width, d.Header.Height)
	}
	return d, nil
}

// Decode reads all chunks from r. Chunks that were decoded fully or partially are
// returned along with any errors.
func Decode(r io.Reader, opts *Options) (*File, error) {
	d, err := NewDecoder(r, opts)
	if err != nil {
		return nil, err
	}
	f := &File{
		Header: d.Header,
	}
	var errs []error
	for c, err := range d.Chunks() {
		if err != nil {
			errs = append(errs, err)
		}
		if c != nil {
			f.Chunks = append(f.Chunks, c)
		}
	}
	return f, errors.Join(errs...)
}

// Chunk is a decoded FLIC chunk.
type Chunk interface {
	ChunkType() ChunkType
	String() string
	SubChunks() []Chunk
}

// UnknownChunk is a chunk of a type the decoder doesn't handle, with its raw data.
type UnknownChunk struct {
	Type ChunkType
	Data []byte
}

func (c *UnknownChunk) ChunkType() ChunkType {
	return c.Type
}

func (c *UnknownChunk) String() string {
	return fmt.Sprintf("Unknown %v data size %d", c.Type, len(c.Data))
}

func (c *UnknownChunk) SubChunks() []Chunk {
	return nil
}

// FrameChunkHeader is the header of a frame chunk.
type FrameChunkHeader struct {
	SubChunks uint16 // Number of subchunks
	Delay     uint16 // Delay in milliseconds
	Reserved  uint16 // Always zero
	Width     uint16 // Frame width override (if non-zero)
	Height    uint16 // Frame height override (if non-zero)
}

// FrameChunk is one frame of the animation. Its subchunks update the palette
// and the image.
type FrameChunk struct {
	Header FrameChunkHeader
	Chunks []Chunk
}

func (c *FrameChunk) ChunkType() ChunkType {
	return ChunkFrame
}

func (c *FrameChunk) String() string {
	return fmt.Sprintf("Frame nSubChunks=%d delay=%d %dx%d", c.Header.SubChunks, c.Header.Delay, c.Header.Width, c.Header.Height)
}

func (c *FrameChunk) SubChunks() []Chunk {
	return c.Chunks
}

// PrefixChunk is the optional FLC prefix chunk with Animator Pro settings, kept
// as raw data.
type PrefixChunk struct {
	Data []byte
}

func (c *PrefixChunk) ChunkType() ChunkType {
	return ChunkPrefix
}

func (c *PrefixChunk) String() string {
	return fmt.Sprintf("Prefix size=%d", len(c.Data))
}

func (c *PrefixChunk) SubChunks() []Chunk {
	return nil
}

// PaletteUpdate is one run of palette entries in a ColorChunk.
type PaletteUpdate struct {
	// Offset is the offset into the palette to apply the update.
	Offset int
	// Colors is the set of palette colors to update starting at Offset.
	Colors []color.NRGBA
}

// ColorChunk updates the palette.
type ColorChunk struct {
	// HighColor is true if this is a Color 256 chunk and false for Color 64.
	HighColor bool
	Updates   []PaletteUpdate
}

func (c *ColorChunk) ChunkType() ChunkType {
	if c.HighColor {
		return ChunkColor256
	}
	return ChunkColor64
}

func (c *ColorChunk) String() string {
	if c.HighColor {
		return fmt.Sprintf("Color256 updates=%d", len(c.Updates))
	}
	return fmt.Sprintf("Color64 updates=%d", len(c.Updates))
}

func (c *ColorChunk) SubChunks() []Chunk {
	return nil
}

// DrawOp writes a run of pixels starting at X, Y.
type DrawOp struct {
	X, Y int
	// Count is how many times Value is written. A literal run has a Count of 1
	// and holds all its pixels in Value.
	Count int
	Value []byte
}

// DrawChunk is a byte-run or delta chunk decoded into draw operations.
type DrawChunk struct {
	Type ChunkType
	Ops  []DrawOp
}

func (c *DrawChunk) ChunkType() ChunkType {
	return c.Type
}

func (c *DrawChunk) String() string {
	return fmt.Sprintf("Draw %v ops=%d", c.Type, len(c.Ops))
}

func (c *DrawChunk) SubChunks() []Chunk {
	return nil
}

// FLICopyChunk holds an uncompressed image, stored row by row.
type FLICopyChunk struct {
	Width, Height int
	Bitmap        []byte
}

func (c *FLICopyChunk) ChunkType() ChunkType {
	return ChunkFLICopy
}

func (c *FLICopyChunk) String() string {
	return "FLICopy"
}

func (c *FLICopyChunk) SubChunks() []Chunk {
	return nil
}

// BlackChunk sets every pixel to palette index 0.
type BlackChunk struct{}

func (c *BlackChunk) ChunkType() ChunkType { return ChunkBlack }
func (c *BlackChunk) String() string       { return "Black" }
func (c *BlackChunk) SubChunks() []Chunk   { return nil }

// PostageStampChunk is a thumbnail of the first frame.
type PostageStampChunk struct {
	Height uint16
	Width  uint16
	Xlate  uint16
	Chunk  Chunk
}

func (c *PostageStampChunk) ChunkType() ChunkType {
	return ChunkPostageStamp
}

func (c *PostageStampChunk) String() string {
	return "PostageStamp"
}

func (c *PostageStampChunk) SubChunks() []Chunk {
	return []Chunk{c.Chunk}
}

// errEOF marks a clean end of file between top-level chunks, as opposed to an
// EOF inside a chunk.
var errEOF = errors.New("EOF")

// ReadChunk reads the next top-level chunk. It returns io.EOF at the end of the
// file. A partially decoded chunk is returned along with its error.
func (d *Decoder) ReadChunk() (Chunk, error) {
	var skipErr error
	if d.resync {
		start := d.offset()
		n, err := d.skipInvalid()
		if n > 0 {
			skipErr = &SkippedDataError{Offset: start, Length: n}
		}
		if err != nil {
			if skipErr != nil {
				return nil, skipErr
			}
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("flic: chunk at offset %d: %w", d.offset(), err)
		}
	}
	start := d.offset()
	c, err := d.readChunk(d.r, true)
	if errors.Is(err, errEOF) {
		if skipErr != nil {
			return nil, skipErr
		}
		return nil, io.EOF
	}
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		err = fmt.Errorf("flic: chunk at offset %d: %w", start, err)
	}
	return c, errors.Join(skipErr, err)
}

// offset returns the file offset of the next unread byte.
func (d *Decoder) offset() int64 {
	return d.cr.n - int64(d.r.Buffered())
}

// skipInvalid discards bytes until the input starts with a plausible top-level
// chunk and returns how many bytes it discarded. It returns io.EOF if it reaches
// the end of the input.
func (d *Decoder) skipInvalid() (int64, error) {
	var n int64
	for {
		b, err := d.r.Peek(frameProbeSize)
		if len(b) == 0 {
			if errors.Is(err, io.EOF) {
				return n, io.EOF
			}
			return n, err
		}
		if plausibleChunk(b, d.offset() == headerSize) {
			return n, nil
		}
		if _, err := d.r.Discard(1); err != nil {
			return n, err
		}
		n++
	}
}

// frameProbeSize is enough to check a frame chunk header and the header of its
// first subchunk.
const frameProbeSize = 6 + 10 + 6

// headerSize is the size of the file header, where the first chunk starts.
const headerSize = 128

// plausibleChunk reports whether b starts with a top-level chunk header that is
// unlikely to be random data. Only frames qualify, plus a prefix chunk when
// atStart is set since a prefix can only come directly after the file header.
func plausibleChunk(b []byte, atStart bool) bool {
	if len(b) < 6 {
		return false
	}
	size := binary.LittleEndian.Uint32(b)
	switch ChunkType(binary.LittleEndian.Uint16(b[4:])) {
	case ChunkPrefix:
		return atStart && size >= 6
	case ChunkFrame:
		if len(b) < 16 || size < 16 {
			return false
		}
		subChunks := binary.LittleEndian.Uint16(b[6:])
		reserved := binary.LittleEndian.Uint16(b[10:])
		if reserved != 0 {
			return false
		}
		if subChunks == 0 {
			return size == 16
		}
		if uint32(subChunks)*6 > size-16 || len(b) < frameProbeSize {
			return false
		}
		subSize := binary.LittleEndian.Uint32(b[16:])
		subType := ChunkType(binary.LittleEndian.Uint16(b[20:]))
		_, known := chunkTypeName[subType]
		return known && subType < ChunkPrefix && subSize >= 6 && subSize <= size-16
	}
	return false
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

// Chunks returns an iterator over the remaining top-level chunks. Iteration stops at
// end of file or after an error that produced no chunk. A partially decoded chunk is
// yielded along with its error, and iteration continues. The decoder reads its input
// once, so ranging over Chunks again resumes where the previous iteration stopped.
func (d *Decoder) Chunks() iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		for {
			c, err := d.ReadChunk()
			if errors.Is(err, io.EOF) {
				return
			}
			if !yield(c, err) || (err != nil && c == nil) {
				return
			}
		}
	}
}

func (d *Decoder) readChunk(r io.Reader, top bool) (Chunk, error) {
	ch, r, err := startReadingChunk(r)
	if err != nil {
		if top && errors.Is(err, io.EOF) {
			return nil, errEOF
		}
		return nil, err
	}
	defer func() {
		// Make sure to consume the entire chunk even if the parser doesn't.
		_, _ = io.Copy(io.Discard, r)
	}()
	switch ch.Type {
	case ChunkPrefix:
		return chunkOrNil(d.readPrefixChunk(r))
	case ChunkFrame:
		return chunkOrNil(d.readFrameChunk(r))
	case ChunkColor64, ChunkColor256:
		return chunkOrNil(d.readColorChunk(r, ch.Type))
	case ChunkByteRun:
		return chunkOrNil(readByteRunChunk(r, int(d.Header.Width), int(d.Header.Height)))
	case ChunkDeltaFLI:
		return chunkOrNil(readDeltaFLIChunk(r, int(d.Header.Width), int(d.Header.Height)))
	case ChunkDeltaFLC:
		return chunkOrNil(readDeltaFLCChunk(r, int(d.Header.Width), int(d.Header.Height)))
	case ChunkBlack:
		return &BlackChunk{}, nil
	case ChunkFLICopy:
		return chunkOrNil(readFLICopyChunk(r, int(d.Header.Width), int(d.Header.Height)))
	case ChunkPostageStamp:
		return chunkOrNil(d.readPostageStampChunk(r))
	}
	expectedSize := int(ch.Size) - 6
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read unknown chunk %v: %w", ch.Type, err)
	}
	if len(data) < expectedSize {
		return nil, fmt.Errorf("chunk %v of size %d is truncated: %w", ch.Type, ch.Size, io.ErrUnexpectedEOF)
	}
	return &UnknownChunk{
		Type: ch.Type,
		Data: data,
	}, nil
}

// chunkOrNil converts a nil chunk pointer to a nil Chunk interface so that
// callers can compare the returned chunk against nil.
func chunkOrNil[T interface {
	Chunk
	comparable
}](c T, err error) (Chunk, error) {
	var zero T
	if c == zero {
		return nil, err
	}
	return c, err
}

func startReadingChunk(r io.Reader) (chunkHeader, io.Reader, error) {
	var ch chunkHeader
	if err := binary.Read(r, binary.LittleEndian, &ch); err != nil {
		return ch, nil, fmt.Errorf("failed to read chunk header: %w", err)
	}
	if ch.Size < 6 {
		return ch, nil, fmt.Errorf("chunk size invalid, expected >= 6 got %d", ch.Size)
	}
	size := int(ch.Size) - 6
	return ch, io.LimitReader(r, int64(size)), nil
}

func (d *Decoder) readColorChunk(r io.Reader, typ ChunkType) (*ColorChunk, error) {
	var nUpdates uint16
	if err := binary.Read(r, binary.LittleEndian, &nUpdates); err != nil {
		return nil, fmt.Errorf("failed to read header: %w", err)
	}
	// A single update can cover the whole palette, so more than 256 means the data is bad.
	if nUpdates > 256 {
		return nil, fmt.Errorf("too many palette updates (%d) in %v chunk", nUpdates, typ)
	}
	c := &ColorChunk{
		HighColor: typ == ChunkColor256,
		Updates:   make([]PaletteUpdate, 0, nUpdates),
	}
	lowColor := typ == ChunkColor64
	colorOffset := 0
	for range nUpdates {
		if _, err := io.ReadFull(r, d.buf[:2]); err != nil {
			return nil, err
		}
		skipCount, copyCount := d.buf[0], d.buf[1]
		cc := int(copyCount)
		if cc == 0 {
			cc = 256
		}
		colorOffset += int(skipCount)
		if m := colorOffset + cc; m > 256 {
			return nil, fmt.Errorf("%v chunk overruns 256 color palette to %d", typ, m)
		}
		if _, err := io.ReadFull(r, d.buf[:3*cc]); err != nil {
			return nil, err
		}
		colors := make([]color.NRGBA, cc)
		for i := range colors {
			r, g, b := d.buf[i*3+0], d.buf[i*3+1], d.buf[i*3+2]
			if lowColor {
				r, g, b = scaleColor64(r), scaleColor64(g), scaleColor64(b)
			}
			colors[i] = color.NRGBA{R: r, G: g, B: b, A: 255}
		}
		c.Updates = append(c.Updates, PaletteUpdate{
			Offset: colorOffset,
			Colors: colors,
		})
		colorOffset += len(colors)
	}
	return c, nil
}

// scaleColor64 expands a 6-bit color component to the full 8-bit range.
func scaleColor64(v byte) byte {
	return byte(255 * int(min(v, 63)) / 63)
}

func readFLICopyChunk(r io.Reader, width, height int) (*FLICopyChunk, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if exp := width * height; len(b) < exp {
		return nil, fmt.Errorf("FLI copy chunk has size %d but expected %d (%dx%d)", len(b), exp, width, height)
	}
	return &FLICopyChunk{Width: width, Height: height, Bitmap: b}, nil
}

func (d *Decoder) readPostageStampChunk(r io.Reader) (*PostageStampChunk, error) {
	/*
	   A "postage stamp" is a reduced image of the first frame of an animation. A postage stamp created by Autodesk Animator Pro has the preferred size of 100*63 (the actual size can vary) and uses a universal palette.

	   The palette is an RGB cube with 6 levels of each red, green and blue. This totals 216 colors. An arbitrary RGB value (where each component is in the range of 0-255) is mapped into the six-cube space using:

	   ((6*red)/256) * 36 + ((6*green)/256) * 6 + (6*blue)/256
	   The PSTAMP chunk embeds another chunk. This is usually a BYTE_RUN chunk or a FLI_COPY chunk (for compressed and uncompressed images respectively). If the type of the embedded chunk is 18 again (the type of PSTAMP is also 18), that chunk contains a 256-byte color translation table. The translation table maps every palette index of the first frame to the universal six-cube color space. To display the postage stamp, a program decodes the image of the first frame and maps every pixel of that image to the six-cube color palette (using the translation table).
	*/

	var h struct {
		Height uint16
		Width  uint16
		Xlate  uint16
	}
	if err := binary.Read(r, binary.LittleEndian, &h); err != nil {
		return nil, fmt.Errorf("failed to read header: %w", err)
	}
	ch, r, err := startReadingChunk(r)
	if err != nil {
		return nil, err
	}
	size := int(ch.Size) - 6
	var subChunk Chunk
	switch ch.Type {
	case ChunkByteRun:
		subChunk, err = readByteRunChunk(r, int(h.Width), int(h.Height))
		if err != nil {
			return nil, err
		}
	case ChunkFLICopy:
		subChunk, err = readFLICopyChunk(r, int(h.Width), int(h.Height))
		if err != nil {
			return nil, err
		}
	case ChunkPostageStamp:
		// A nested postage stamp is a 256 color translation table, which isn't supported.
		return nil, errors.New("unsupported postage stamp palette remapping")
	default:
		// Read rather than allocate size bytes up front, since size comes from the file.
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("failed to read unknown chunk %v: %w", ch.Type, err)
		}
		if len(data) < size {
			return nil, fmt.Errorf("unknown chunk %v of size %d is truncated: %w", ch.Type, ch.Size, io.ErrUnexpectedEOF)
		}
		subChunk = &UnknownChunk{Type: ch.Type, Data: data}
	}
	return &PostageStampChunk{
		Width:  h.Width,
		Height: h.Height,
		Xlate:  h.Xlate,
		Chunk:  subChunk,
	}, err
}

func readByteRunChunk(r io.Reader, width, height int) (*DrawChunk, error) {
	// Not d.buf, since the returned ops reference this data.
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	ops, err := decodeByteRunData(data, width, height)
	return &DrawChunk{Type: ChunkByteRun, Ops: ops}, err
}

func readDeltaFLIChunk(r io.Reader, width, height int) (*DrawChunk, error) {
	// Not d.buf, since the returned ops reference this data.
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	ops, err := decodeDeltaFLI(data, width, height)
	return &DrawChunk{Type: ChunkDeltaFLI, Ops: ops}, err
}

func readDeltaFLCChunk(r io.Reader, width, height int) (*DrawChunk, error) {
	// Not d.buf, since the returned ops reference this data.
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	ops, err := decodeDeltaFLC(data, width, height)
	return &DrawChunk{Type: ChunkDeltaFLC, Ops: ops}, err
}

func (d *Decoder) readPrefixChunk(r io.Reader) (*PrefixChunk, error) {
	/*
		TODO: decode this properly
		The documentation seems to be wrong about the format here. Instead of a count of subchunks,
		it seems that directly after the prefix chunk header comes "Celdata" after which comes the file path.

		https://github.com/AnimatorPro/Animator-Pro/blob/6d0b68cd94bb5cfde2cdd05e9a7c8ee1e1cb3cbb/Pristine-Pro/FLICELIO.C#L38
		https://github.com/AnimatorPro/Animator-Pro/blob/6d0b68cd94bb5cfde2cdd05e9a7c8ee1e1cb3cbb/Pristine-Pro/INC/FLICEL.H#L62
		https://github.com/AnimatorPro/Animator-Pro/blob/6d0b68cd94bb5cfde2cdd05e9a7c8ee1e1cb3cbb/src/PJ/inc/vmagics.h#L40

		size: uint32
		type: uint16 // 2
		version: uint16 // this is always zero it seems
		cdatPosCenterXY: [2]int16 // center of cel
		cdatPosStretchXY: [2]int16 // amount added to cel width and height, for width, if <= -width cel is flipped and width is amount < -width
		cdatPosRotAngXYZ: [3]int16 // angle of rotation 0 to 5760 == 0 to 360 degrees for x y and z
		curFrame: uint16 // current frame in cel file
		nextFrameOffset: uint32 // offset to next (after current) frame in fli
		tcolor: uint16 // pre translation tcolor for this cel
		/ * total size 66 bytes * /
		UBYTE cdpad[64 - POSTOSET(struct celdata,next_frame_oset)];

		 -- though this doesn't exactly match what it seems to be, the size and type are correct though

		 Prefix sub chunk types:
		 https://github.com/AnimatorPro/Animator-Pro/blob/6d0b68cd94bb5cfde2cdd05e9a7c8ee1e1cb3cbb/src/PJ/poco/poekit/flicplay/fli.h#L39
		FP_FREE 		= 0,			//	unused space
		FP_FLIPATH		= 1,			//	fli path, valid in a cel or flx file
		FP_VSETTINGS	= 2,			//	vsettings chunk
			https://github.com/AnimatorPro/Animator-Pro/blob/6d0b68cd94bb5cfde2cdd05e9a7c8ee1e1cb3cbb/Pristine-Pro/VSETTING.C
			https://github.com/AnimatorPro/Animator-Pro/blob/6d0b68cd94bb5cfde2cdd05e9a7c8ee1e1cb3cbb/src/PJ/inc/vsetfile.h#L24
			https://github.com/AnimatorPro/Animator-Pro/blob/6d0b68cd94bb5cfde2cdd05e9a7c8ee1e1cb3cbb/Pristine-Pro/INC/VSETFILE.H#L21
			https://github.com/AnimatorPro/Animator-Pro/blob/6d0b68cd94bb5cfde2cdd05e9a7c8ee1e1cb3cbb/src/V/globals.c#L10
			https://github.com/AnimatorPro/Animator-Pro/blob/6d0b68cd94bb5cfde2cdd05e9a7c8ee1e1cb3cbb/src/V/jimk.h#L438
			https://github.com/AnimatorPro/Animator-Pro/blob/6d0b68cd94bb5cfde2cdd05e9a7c8ee1e1cb3cbb/src/PLAY/jimk.h#L261
		FP_CELDATA		= 3,			//	cel information chunk
	*/
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return &PrefixChunk{Data: b}, nil
}

func (d *Decoder) readFrameChunk(r io.Reader) (*FrameChunk, error) {
	c := &FrameChunk{}
	if err := binary.Read(r, binary.LittleEndian, &c.Header); err != nil {
		return nil, fmt.Errorf("failed to read subchunk header: %w", err)
	}
	c.Chunks = make([]Chunk, 0, c.Header.SubChunks)
	var errs []error
	for range c.Header.SubChunks {
		sc, err := d.readChunk(r, false)
		if err != nil {
			errs = append(errs, err)
			if sc == nil {
				return nil, errors.Join(errs...)
			}
		}
		c.Chunks = append(c.Chunks, sc)
	}
	return c, errors.Join(errs...)
}

func decodeByteRunData(data []byte, width, height int) ([]DrawOp, error) {
	var ops []DrawOp
	off := 0
	for y := range height {
		if off >= len(data) {
			return nil, errors.New("rle data premature end")
		}
		// Ignore the packet count since there can be more than 255 packets.
		off++
		for x := 0; x < width; {
			if off >= len(data) {
				return nil, errors.New("rle data premature end")
			}
			n := int(int8(data[off]))
			off++
			op := DrawOp{X: x, Y: y}
			switch {
			case n < 0:
				// Literal run of n pixels.
				n = -n
				if len(data)-off < n {
					return nil, errors.New("rle data premature end")
				}
				op.Count = 1
				op.Value = data[off : off+min(n, width-x)]
				off += n
				x += n
			case n > 0:
				// Single pixel repeated n times.
				if off >= len(data) {
					return nil, errors.New("rle data premature end")
				}
				op.Count = min(n, width-x)
				op.Value = data[off : off+1]
				off++
				x += n
			default:
				// A zero length packet makes no progress, so treat it as end of line.
				x = width
				continue
			}
			ops = append(ops, op)
		}
	}
	return ops, nil
}

func decodeDeltaFLI(data []byte, width, height int) ([]DrawOp, error) {
	if len(data) < 4 {
		return nil, errors.New("delta fli premature end")
	}
	startLine := int(data[0]) | (int(data[1]) << 8)
	lineCount := int(data[2]) | (int(data[3]) << 8)
	if startLine >= height {
		return nil, fmt.Errorf("delta FLI start line %d greater than image height %d", startLine, height)
	}
	if startLine+lineCount > height {
		return nil, fmt.Errorf("delta FLI start line %d + line count %d greater than image height %d", startLine, lineCount, height)
	}
	off := 4

	var ops []DrawOp
	for y := startLine; y < startLine+lineCount; y++ {
		if off >= len(data) {
			return nil, errors.New("delta fli premature end")
		}
		packetCount := data[off]
		off++
		var x int
		for range int(packetCount) {
			if off+1 >= len(data) {
				return nil, errors.New("delta fli premature end")
			}
			colSkip := int(data[off])
			n := int(int8(data[off+1]))
			off += 2
			x += colSkip
			if x >= width {
				// Corrupt data; return what we have.
				return ops, nil
			}
			op := DrawOp{X: x, Y: y}
			switch {
			case n > 0:
				// Literal run of n pixels.
				if len(data)-off < n {
					return nil, errors.New("delta fli premature end")
				}
				op.Count = 1
				op.Value = data[off : off+min(n, width-x)]
				off += n
				x += n
			case n < 0:
				// Single pixel repeated -n times.
				if off >= len(data) {
					return nil, errors.New("delta fli premature end")
				}
				op.Count = min(-n, width-x)
				op.Value = data[off : off+1]
				off++
				x += -n
			default:
				continue
			}
			ops = append(ops, op)
		}
	}
	return ops, nil
}

func decodeDeltaFLC(data []byte, width, height int) ([]DrawOp, error) {
	if len(data) <= 2 {
		return nil, errors.New("delta flc premature end")
	}
	lineCount := int(data[0]) | (int(data[1]) << 8)
	if lineCount > height {
		return nil, fmt.Errorf("delta FLC line count %d greater than image height %d", lineCount, height)
	}
	off := 2

	var ops []DrawOp
	y := 0
	for range lineCount {
		var packetCount int
	opLoop:
		for {
			if off+1 >= len(data) {
				return nil, errors.New("delta flc premature end")
			}
			op := uint16(data[off]) | (uint16(data[off+1]) << 8)
			off += 2
			switch op >> 14 {
			case 0:
				packetCount = int(op)
				break opLoop
			case 1:
				return nil, errors.New("undefined op code found in FLC delta chunk")
			case 2:
				// Last pixel of the current line. Needed for odd widths since the
				// packets below are word oriented.
				if y >= 0 && y < height {
					ops = append(ops, DrawOp{
						X:     width - 1,
						Y:     y,
						Count: 1,
						Value: []byte{byte(op & 0xff)},
					})
				}
			case 3:
				// The op holds a negative line skip count.
				y -= int(int16(op))
			}
		}
		if y < 0 || y >= height {
			// Corrupt data; return what we have.
			return ops, nil
		}
		if packetCount == 0 {
			y++
			continue
		}
		var x int
		for range packetCount {
			if off+1 >= len(data) {
				return nil, errors.New("delta flc premature end")
			}
			colSkip := int(data[off])
			n := int(int8(data[off+1]))
			off += 2
			x += colSkip
			if x >= width {
				// Corrupt data; return what we have.
				return ops, nil
			}
			op := DrawOp{X: x, Y: y}
			switch {
			case n > 0:
				// Literal run of n words.
				if len(data)-off < n*2 {
					return nil, errors.New("delta flc premature end")
				}
				op.Count = 1
				op.Value = data[off : off+min(n*2, width-x)]
				off += n * 2
				x += n * 2
			case n < 0:
				// Single word repeated -n times.
				if len(data)-off < 2 {
					return nil, errors.New("delta flc premature end")
				}
				n = -n
				// Allow the last word to straddle the end of the scanline.
				op.Count = min(n, (width-x+1)/2)
				op.Value = data[off : off+2]
				off += 2
				x += n * 2
			default:
				continue
			}
			ops = append(ops, op)
		}
		y++
	}
	return ops, nil
}
