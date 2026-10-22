# Test data

A small set of real FLIC files that together cover the main parts of the format.

| File | Size | Dimensions | Covers |
| --- | --- | --- | --- |
| `phasers.fli` | 86K | 320x200 | FLI (0xAF11) header, Color64 palette, ByteRun full frame, DeltaFLI delta frames (line skip, literal and RLE packets). Damaged: the header says 263 frames, but 60 frames are followed by 64 KB of non-FLIC data at offset 16484 (it looks like 8-bit audio), then 38 more frames, the last one cut short. Decoding stops with an error at the bad data unless `Options.Resync` is set, which skips it. |
| `ship.flc` | 57K | 640x480 | FLC (0xAF12) header, prefix chunk, postage stamp, Color256 palette, ByteRun full frame, DeltaFLC delta frames. |
| `alex.flc` | 36K | 128x100 | The DeltaFLC last-pixel op, which no other sample uses, odd dimensions, and no prefix or postage stamp chunk. Cut down to the first three frames of a 580K file, with the header size and frame count edited, so its third frame is a real frame and not a ring frame. |

Black and FLICopy chunks have no real sample here and are covered by unit tests in
`flic_test.go`.
