// gen_icon generates icon.png for the Claudy tray app.
// Run: go run ./pkg/assets/gen_icon/
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

func main() {
	const size = 64
	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	bg := color.NRGBA{0, 0, 0, 0}
	white := color.NRGBA{255, 255, 255, 255}
	red := color.NRGBA{192, 58, 58, 255}

	// Fill transparent
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.SetNRGBA(x, y, bg)
		}
	}

	// Draw cloud shape (pixel circles combined)
	circles := []struct{ cx, cy, r float64 }{
		{20, 38, 12},
		{32, 30, 14},
		{44, 36, 10},
		{14, 40, 8},
		{50, 38, 8},
	}
	setIfCloud := func(x, y int) bool {
		for _, c := range circles {
			dx := float64(x) - c.cx
			dy := float64(y) - c.cy
			if math.Sqrt(dx*dx+dy*dy) <= c.r {
				return true
			}
		}
		// Fill bottom rectangle
		if float64(y) >= 37 && float64(x) >= 14 && float64(x) <= 54 && float64(y) <= 50 {
			return true
		}
		return false
	}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if setIfCloud(x, y) {
				img.SetNRGBA(x, y, white)
			}
		}
	}

	// Draw glasses (two small red rectangles + bridge)
	// Left lens: x 18-27, y 37-43
	for y := 37; y <= 43; y++ {
		for x := 18; x <= 27; x++ {
			if x == 18 || x == 27 || y == 37 || y == 43 {
				img.SetNRGBA(x, y, red)
			}
		}
	}
	// Right lens: x 31-40, y 37-43
	for y := 37; y <= 43; y++ {
		for x := 31; x <= 40; x++ {
			if x == 31 || x == 40 || y == 37 || y == 43 {
				img.SetNRGBA(x, y, red)
			}
		}
	}
	// Bridge
	for x := 28; x <= 30; x++ {
		img.SetNRGBA(x, 40, red)
	}
	// Left arm
	img.SetNRGBA(16, 40, red)
	img.SetNRGBA(17, 40, red)
	// Right arm
	img.SetNRGBA(41, 40, red)
	img.SetNRGBA(42, 40, red)

	f, _ := os.Create("pkg/assets/icon.png")
	defer f.Close()
	png.Encode(f, img)
}
