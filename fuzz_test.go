package flic

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func addSeeds(f *testing.F) {
	files, err := filepath.Glob("testdata/*.fl?")
	if err != nil {
		f.Fatal(err)
	}
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
}

// FuzzDecode checks that decoding never panics, with and without resync.
func FuzzDecode(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = Decode(bytes.NewReader(data), nil)
		_, _ = Decode(bytes.NewReader(data), &Options{Resync: true})
	})
}

// maxRenderPixels bounds the pixel memory a fuzz input can make RenderGIF use.
const maxRenderPixels = 64 << 20

// FuzzRenderGIF checks that rendering never panics, with and without resync.
func FuzzRenderGIF(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = RenderGIF(bytes.NewReader(data), &Options{MaxPixels: maxRenderPixels})
		_, _ = RenderGIF(bytes.NewReader(data), &Options{Resync: true, MaxPixels: maxRenderPixels})
	})
}
