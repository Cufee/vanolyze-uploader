//go:build windows

package upload

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

var iconColors = map[string]color.NRGBA{
	"grey": {
		R: 128,
		G: 128,
		B: 128,
		A: 255,
	},
	"blue": {
		R: 66,
		G: 156,
		B: 255,
		A: 255,
	},
	"green": {
		R: 38,
		G: 199,
		B: 130,
		A: 255,
	},
	"amber": {
		R: 245,
		G: 180,
		B: 51,
		A: 255,
	},
	"red": {
		R: 239,
		G: 85,
		B: 96,
		A: 255,
	},
}

// makeTrayIcon draws a white upload arrow on a colored disk, as an ICO file.
func makeTrayIcon(background color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))

	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			dx, dy := x-16, y-16
			if dx*dx+dy*dy <= 225 {
				img.SetNRGBA(x, y, background)
			}

			arrow := (x >= 14 && x <= 18 && y >= 10 && y <= 22) ||
				(y >= 8 && y <= 15 && x >= 16-(y-8) && x <= 16+(y-8)) ||
				(y >= 24 && y <= 26 && x >= 9 && x <= 23)
			if arrow {
				img.SetNRGBA(x, y, color.NRGBA{
					255,
					255,
					255,
					255,
				})
			}
		}
	}

	var pngData bytes.Buffer

	_ = png.Encode(&pngData, img)

	var ico bytes.Buffer
	for _, v := range []uint16{
		0,
		1,
		1,
	} {
		_ = binary.Write(&ico, binary.LittleEndian, v)
	}

	ico.Write([]byte{
		32,
		32,
		0,
		0,
	})

	for _, v := range []uint16{
		1,
		32,
	} {
		_ = binary.Write(&ico, binary.LittleEndian, v)
	}

	_ = binary.Write(&ico, binary.LittleEndian, uint32(pngData.Len()))
	_ = binary.Write(&ico, binary.LittleEndian, uint32(22))
	ico.Write(pngData.Bytes())

	return ico.Bytes()
}
