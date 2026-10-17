package main

import (
	"bytes"
	"image/png"
	"testing"
)

func TestTrayIconDrawsABolt(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(trayIcon(true)))
	if err != nil {
		t.Fatalf("decode the template icon: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 36 || b.Dy() != 36 {
		t.Fatalf("the icon is %dx%d, want 36x36", b.Dx(), b.Dy())
	}
	// The bolt's waist crosses the vertical middle, so the middle column
	// must carry ink.
	ink := 0
	for y := 0; y < 36; y++ {
		if r, _, _, a := img.At(18, y).RGBA(); a > 0 && r < 0x1000 {
			ink++
		}
	}
	if ink == 0 {
		t.Error("no ink on the icon's middle column; the bolt did not draw")
	}
	// A gray fallback exists for the platforms that do not tint templates.
	if _, err := png.Decode(bytes.NewReader(trayIcon(false))); err != nil {
		t.Fatalf("decode the plain icon: %v", err)
	}
}
