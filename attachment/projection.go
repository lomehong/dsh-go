package attachment

import "math"

// RequestImageDimensions computes aspect-preserving integer dimensions within
// a hard total-pixel budget. The result rounds inward; small images are not
// enlarged.
func RequestImageDimensions(width, height, maxPixels int) ImageDimensions {
	scale := math.Min(1, math.Sqrt(float64(maxPixels)/float64(width*height)))
	if scale == 1 {
		return ImageDimensions{Width: width, Height: height}
	}
	if width >= height {
		projectedWidth := int(math.Max(1, math.Floor(float64(width)*scale)))
		projectedHeight := int(math.Max(1, math.Round(float64(projectedWidth)*float64(height)/float64(width))))
		for projectedWidth*projectedHeight > maxPixels && projectedWidth > 1 {
			projectedWidth--
			projectedHeight = int(math.Max(1, math.Round(float64(projectedWidth)*float64(height)/float64(width))))
		}
		return ImageDimensions{Width: projectedWidth, Height: projectedHeight}
	}
	projectedHeight := int(math.Max(1, math.Floor(float64(height)*scale)))
	projectedWidth := int(math.Max(1, math.Round(float64(projectedHeight)*float64(width)/float64(height))))
	for projectedWidth*projectedHeight > maxPixels && projectedHeight > 1 {
		projectedHeight--
		projectedWidth = int(math.Max(1, math.Round(float64(projectedHeight)*float64(width)/float64(height))))
	}
	return ImageDimensions{Width: projectedWidth, Height: projectedHeight}
}
