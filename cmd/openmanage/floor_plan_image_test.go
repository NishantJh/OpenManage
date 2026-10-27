package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodedImage(t *testing.T, w, h int, asJPEG bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.White)
	var b bytes.Buffer
	var err error
	if asJPEG {
		err = jpeg.Encode(&b, img, nil)
	} else {
		err = png.Encode(&b, img)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSniffImage(t *testing.T) {
	ct, w, h, err := sniffImage(encodedImage(t, 40, 20, false))
	if err != nil || ct != "image/png" || w != 40 || h != 20 {
		t.Fatalf("png: %q %dx%d %v", ct, w, h, err)
	}
	ct, w, h, err = sniffImage(encodedImage(t, 30, 30, true))
	if err != nil || ct != "image/jpeg" || w != 30 || h != 30 {
		t.Fatalf("jpeg: %q %dx%d %v", ct, w, h, err)
	}
	png := encodedImage(t, 8, 8, false)
	bad := map[string][]byte{
		"svg":           []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"html":          []byte("<html><body>hi</body></html>"),
		"gif":           []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"empty":         {},
		"truncated png": png[:20],
		"too wide":      encodedImage(t, floorImageMaxSide+1, 1, false),
		"too stretched": encodedImage(t, 200, 10, false),
	}
	for name, data := range bad {
		if _, _, _, err := sniffImage(data); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestFloorImageHeight(t *testing.T) {
	i := floorImage{WidthM: 20, PxW: 800, PxH: 500}
	if got := i.heightM(); got != 12.5 {
		t.Fatalf("height = %v, want 12.5", got)
	}
	if (floorImage{WidthM: 20}).heightM() != 0 {
		t.Fatal("an image with unknown pixels should have no height")
	}
}

func TestFloorImageCrop(t *testing.T) {
	i := floorImage{WidthM: 10, PxW: 800, PxH: 400, Crop: &floorCrop{X: 0.25, Y: 0, W: 0.5, H: 0.5}}
	if got := i.heightM(); got != 5 {
		t.Fatalf("cropped height = %v, want 5", got)
	}
	bad := []floorCrop{{X: -0.1, W: 0.5, H: 0.5}, {X: 0.6, W: 0.5, H: 0.5}, {W: 0.01, H: 0.5}, {Y: 0.9, W: 0.5, H: 0.2}}
	for _, c := range bad {
		c := c
		img := floorImage{WidthM: 10, Crop: &c}
		if img.validate() == nil {
			t.Fatalf("crop %+v should be rejected", c)
		}
	}
	ok := floorImage{WidthM: 10, Crop: &floorCrop{X: 0.5, Y: 0.5, W: 0.5, H: 0.5}}
	if err := ok.validate(); err != nil {
		t.Fatalf("crop to the bottom-right quarter should pass: %v", err)
	}
}
