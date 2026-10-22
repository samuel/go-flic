package flic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"image/color"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/golden.json from the current output")

const goldenPath = "testdata/golden.json"

type goldenFrame struct {
	Delay int    `json:"delay"`
	Hash  string `json:"hash"` // SHA-256 of the palette's RGBA values followed by the pixels
}

// TestGolden renders every test file and compares each frame against
// testdata/golden.json. Run with -update after an intended change to the output.
func TestGolden(t *testing.T) {
	files, err := filepath.Glob("testdata/*.fl?")
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string][]goldenFrame)
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		// Damaged files are expected to fail partway; their frames are still checked.
		g, _ := RenderGIF(f, nil)
		_ = f.Close() // Opened read-only.
		if g == nil {
			t.Fatalf("%s: no frames rendered", path)
		}
		var frames []goldenFrame
		for i, im := range g.Image {
			h := sha256.New()
			for _, c := range im.Palette {
				nc := color.NRGBAModel.Convert(c).(color.NRGBA)
				h.Write([]byte{nc.R, nc.G, nc.B, nc.A})
			}
			h.Write(im.Pix)
			frames = append(frames, goldenFrame{Delay: g.Delay[i], Hash: hex.EncodeToString(h.Sum(nil))})
		}
		got[filepath.Base(path)] = frames
	}

	if *update {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	b, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%s (run with -update to create it)", err)
	}
	var want map[string][]goldenFrame
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	for name, frames := range got {
		wantFrames, ok := want[name]
		if !ok {
			t.Errorf("%s: missing from %s", name, goldenPath)
			continue
		}
		if len(frames) != len(wantFrames) {
			t.Errorf("%s: want %d frames, got %d", name, len(wantFrames), len(frames))
			continue
		}
		for i := range frames {
			if frames[i] != wantFrames[i] {
				t.Errorf("%s: frame %d: want %+v, got %+v", name, i, wantFrames[i], frames[i])
			}
		}
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("%s: in %s but not in testdata", name, goldenPath)
		}
	}
}
