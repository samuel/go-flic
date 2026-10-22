// Command flic2gif converts a FLIC animation (.fli or .flc) to an animated GIF.
//
// Usage:
//
//	flic2gif [-o out.gif] [-resync] in.flc
//
// The output defaults to the input name with a .gif extension. If the input is
// damaged, the frames that could be rendered are still written, the error is
// logged, and the exit status is 1.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"samuelks.com/flic"
	"samuelks.com/gifopt"
)

func main() {
	out := flag.String("o", "", "output file (default: input name with a .gif extension)")
	resync := flag.Bool("resync", false, "skip damaged data instead of stopping at it")
	flag.Usage = func() {
		//nolint:errcheck // Nothing to do if usage can't be printed.
		fmt.Fprintf(flag.CommandLine.Output(), "usage: flic2gif [-o out.gif] [-resync] in.flc\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	in := flag.Arg(0)
	if *out == "" {
		*out = strings.TrimSuffix(in, filepath.Ext(in)) + ".gif"
	}
	if err := run(in, *out, *resync); err != nil {
		slog.Error("flic2gif", "input", in, "err", err)
		os.Exit(1)
	}
}

func run(in, out string, resync bool) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	//nolint:errcheck // Opened read-only.
	defer f.Close()

	g, renderErr := flic.RenderGIF(f, &flic.Options{Resync: resync})
	if g == nil {
		return renderErr
	}
	// OptimizeGIF merges identical frames, so count them first.
	frames := len(g.Image)

	fo, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := gifopt.OptimizeGIF(fo, g, nil); err != nil {
		_ = fo.Close() // The write error is the one to report.
		return fmt.Errorf("writing %s: %w", out, err)
	}
	if err := fo.Close(); err != nil {
		return err
	}
	if renderErr != nil {
		return fmt.Errorf("wrote %d frames to %s, but the input is damaged: %w", frames, out, renderErr)
	}
	return nil
}
