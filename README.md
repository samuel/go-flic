# go-flic

[![Go Reference](https://pkg.go.dev/badge/samuelks.com/flic.svg)](https://pkg.go.dev/samuelks.com/flic)

Go library for decoding FLIC animation files (`.fli`, `.flc`) and converting them to animated GIF.

FLIC is the animation format used by Autodesk Animator and Animator Pro. `.fli` files use the older, lower-resolution format; `.flc` files support higher resolutions, per-frame palettes, and additional compression schemes.

## Command line

`flic2gif` converts a FLIC file to an optimized GIF:

```sh
go install samuelks.com/flic/cmd/flic2gif@latest
flic2gif anim.flc                 # writes anim.gif
flic2gif -o out.gif anim.flc
flic2gif -resync damaged.fli      # skip damaged data
```

If the input is damaged, `flic2gif` still writes the frames it could render, logs the error, and exits with status 1.

## Usage

```sh
go get samuelks.com/flic
```

```go
import "samuelks.com/flic"
```

### Decoding

Parse a complete file into a chunk tree:

```go
f, err := os.Open("anim.flc")
if err != nil {
    return err
}
defer f.Close()

file, err := flic.Decode(f, nil)
if err != nil {
    return err
}
for _, c := range file.Chunks {
    handleChunk(c)
}
```

`Decode` returns every chunk it could decode, including partial ones, so `file` is usable even when `err` is not nil.

To render as you read, or to avoid holding the whole file in memory, stream chunks with `NewDecoder` and `Chunks`:

```go
d, err := flic.NewDecoder(f, nil)
if err != nil {
    return err
}
for c, err := range d.Chunks() {
    if err != nil {
        return err
    }
    handleChunk(c)
}
```

Iteration stops at the end of the file. A partially decoded chunk comes with its error and iteration continues; an error without a chunk ends iteration. A chunk that runs past the end of the file gives an error wrapping `io.ErrUnexpectedEOF` that includes the chunk's offset. `ReadChunk` reads one chunk at a time and returns `io.EOF` at the end of the file.

`Chunk` is an interface. Concrete types include `*FrameChunk` (containing subchunks), `*DrawChunk` (RLE byte-run/delta ops), `*ColorChunk` (palette updates), `*FLICopyChunk`, `*BlackChunk`, and `*PostageStampChunk`. Unrecognized chunk types come back as `*UnknownChunk` with their raw data. `ChunkType()` returns a `flic.ChunkType`, which can be compared against constants such as `flic.ChunkByteRun` and `flic.ChunkDeltaFLC`, and prints as the chunk type's name.

The `Header` holds file metadata (dimensions, frame count, depth, speed). `Header.FrameDelay()` converts the speed to a `time.Duration`, since FLI uses 1/70 s units and FLC uses milliseconds.

### Damaged files

By default decoding stops at the first chunk that can't be read. To skip over damage instead, pass `&flic.Options{Resync: true}` to `NewDecoder`, `Decode`, or `RenderGIF`. The decoder then discards bytes until it finds what looks like a frame header. Each skip is reported as a `*flic.SkippedDataError`, with the offset and length of the skipped data, alongside the chunk that follows it:

```go
g, err := flic.RenderGIF(f, &flic.Options{Resync: true})
var skipped *flic.SkippedDataError
if errors.As(err, &skipped) {
    log.Printf("skipped %d bytes at offset %d", skipped.Length, skipped.Offset)
}
```

Resync is off by default because recovered frames may not look right. Each delta frame changes the frame before it, so if the damage replaced frames, the frames after it are drawn on the wrong image.

### Rendering to GIF

```go
g, err := flic.RenderGIF(f, nil)
if err != nil {
    return err
}
out, err := os.Create("anim.gif")
if err != nil {
    return err
}
defer out.Close()
if err := gif.EncodeAll(out, g); err != nil {
    return err
}
```

`RenderGIF` returns an `*image/gif.GIF` ready for `gif.EncodeAll`. Every frame is a full image, so run the result through an optimizer such as `samuelks.com/gifopt` to shrink it. If the input is damaged, it returns the frames rendered so far along with the error.

Because every frame is a full image, a small file whose header claims large dimensions can need gigabytes of memory. When rendering untrusted input, set `Options.MaxPixels` to cap the total pixels across all frames. Once the next frame would go over the cap, `RenderGIF` stops and returns the frames so far with an error wrapping `flic.ErrPixelLimit`. The default of zero means no limit.

```go
g, err := flic.RenderGIF(f, &flic.Options{MaxPixels: 100 << 20})
```

A FLIC ends with a ring frame that brings the animation back to its first frame. A GIF loops on its own, so `RenderGIF` leaves the ring frame out, but only when the file has one more frame than the header says and that frame matches the first one. Delays are rounded to the nearest centisecond, the GIF unit. Unknown chunks are ignored; use `Decode` or `Chunks` to see them.

For lower-level rendering, the `ApplyDrawChunk`, `ApplyColorChunk`, `ApplyFLICopyChunk`, `ApplyBlackChunk`, and `ApplyDrawOps` helpers apply chunks to an `*image.Paletted`, and `RenderPostageStamp` renders a postage-stamp thumbnail using `PostageStampPalette()`.

### Limitations

- Only the classic FLI (0xAF11) and FLC (0xAF12) types are accepted. The EGI extensions (0xAF30, 0xAF31, 0xAF44) are rejected.
- Only 8-bit paletted content is rendered.
- `RenderGIF` ignores the per-frame delay override in FLC frames and uses the header speed for every frame.

## Layout

- `flic.go`: the parser. `NewDecoder`, `Decode`, the `Chunk` types, and the byte-run, delta (FLI/FLC), and palette decoders.
- `render.go`: `RenderGIF`, the `Apply*` functions, and `RenderPostageStamp`.
- `cmd/flic2gif/`: the command-line converter.
- `testdata/`: real FLIC files used by the tests, and `golden.json` with the expected output for each.

## Testing

```sh
go test ./...
go test -run TestGolden -update .           # rewrite testdata/golden.json after an intended output change
go test -run TestDecoder -gifdir /tmp/out . # also write a GIF of each test file to /tmp/out
go test -fuzz=FuzzDecode -fuzztime=2m .     # or FuzzRenderGIF
```

`TestGolden` compares each rendered frame of the test files, by delay and a hash of its palette and pixels, against `testdata/golden.json`.

## References

- https://www.compuphase.com/flic.htm
- http://steve.hollasch.net/cgindex/formats/fli.html
- http://bespin.org/~qz/pc-gpe/fli.for
- https://archive.org/details/AAPlay_1020
- https://github.com/aseprite/flic
- https://github.com/wangds/libflic
- https://github.com/AnimatorPro/Animator-Pro
- http://fastgraph.com/help/flic_header_format.html
- https://github.com/bovine/fli2gif