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
	}
	for name, data := range bad {
		if _, _, _, err := sniffImage(data); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
