package bimg

/*
#cgo pkg-config: vips
#include "vips/vips.h"
*/
import "C"

import (
	"errors"
	"math"
)

func Resize(buf []byte, o Options) ([]byte, error) {
	defer C.vips_thread_shutdown()

	if len(buf) == 0 {
		return nil, errors.New("Image buffer is empty")
	}

	image, imageType, err := vipsRead(buf)
	if err != nil {
		return nil, err
	}

	// Define default options
	applyDefaults(&o, imageType)

	if IsTypeSupported(o.Type) == false {
		return nil, errors.New("Unsupported image output type")
	}

	debug("Options: %#v", o)

	inWidth := int(image.Xsize)
	inHeight := int(image.Ysize)

	// Infer the required operation based on the in/out image sizes for a coherent transformation
	normalizeOperation(&o, inWidth, inHeight)

	// image calculations
	factor := imageCalculations(&o, inWidth, inHeight)
	shrink := calculateShrink(factor, o.Interpolator)
	residual := calculateResidual(factor, shrink)

	// Do not enlarge the output if the input width or height
	// are already less than the required dimensions
	if !o.Enlarge && !o.Force {
		if inWidth < o.Width && inHeight < o.Height {
			factor = 1.0
			shrink = 1
			residual = 0
			o.Width = inWidth
			o.Height = inHeight
		}
	}

	// Try to use libjpeg shrink-on-load
	if imageType == JPEG && shrink >= 2 {
		tmpImage, factor, err := shrinkJpegImage(buf, image, factor, shrink)
		if err != nil {
			return nil, err
		}

		image = tmpImage
		factor = math.Max(factor, 1.0)
		shrink = int(math.Floor(factor))
		residual = float64(shrink) / factor
	}

	// Zoom image, if necessary
	image, err = zoomImage(image, o.Zoom)
	if err != nil {
		return nil, err
	}

	// Rotate / flip image, if necessary
	image, err = rotateAndFlipImage(image, o)
	if err != nil {
		return nil, err
	}

	// Transform image, if necessary
	if shouldTransformImage(o, inWidth, inHeight) {
		image, err = transformImage(image, o, shrink, residual)
		if err != nil {
			return nil, err
		}
	}

	// Apply effects, if necessary
	if shouldApplyEffects(o) {
		image, err = applyEffects(image, o)
		if err != nil {
			return nil, err
		}
	}

	// Add watermark, if necessary
	image, err = watermakImage(image, o.Watermark)
	if err != nil {
		return nil, err
	}

	saveOptions := vipsSaveOptions{
		Quality:        o.Quality,
		Type:           o.Type,
		Compression:    o.Compression,
		Interlace:      o.Interlace,
		NoProfile:      o.NoProfile,
		Interpretation: o.Interpretation,
	}

	// Finally get the resultant buffer
	buf, err = vipsSave(image, saveOptions)
	if err != nil {
		return nil, err
	}

	return buf, nil
}

func applyDefaults(o *Options, imageType ImageType) {
	if o.Quality == 0 {
		o.Quality = QUALITY
	}
	if o.Compression == 0 {
		o.Compression = 6
	}
	if o.Type == 0 {
		o.Type = imageType
	}
	if o.Interpretation == 0 {
		o.Interpretation = INTERPRETATION_sRGB
	}
}

func normalizeOperation(o *Options, inWidth, inHeight int) {
	if !o.Force && !o.Crop && !o.Embed && !o.Enlarge && o.Rotate == 0 && (o.Width > 0 || o.Height > 0) {
		o.Force = true
	}
}

func shouldTransformImage(o Options, inWidth, inHeight int) bool {
	return o.Force || (o.Width > 0 && o.Width != inWidth) ||
		(o.Height > 0 && o.Height != inHeight) || o.AreaWidth > 0 || o.AreaHeight > 0
}

func shouldApplyEffects(o Options) bool {
	return o.GaussianBlur.Sigma > 0 || o.GaussianBlur.MinAmpl > 0
}

func transformImage(image *C.VipsImage, o Options, shrink int, residual float64) (*C.VipsImage, error) {
	var err error

	// Use vips_shrink with the integral reduction
	if shrink > 1 {
		image, residual, err = shrinkImage(image, o, residual, shrink)
		if err != nil {
			return nil, err
		}
	}

	residualx, residualy := residual, residual
	if o.Force {
		residualx = float64(o.Width) / float64(image.Xsize)
		residualy = float64(o.Height) / float64(image.Ysize)
	}

	if o.Force || residual != 0 {
		image, err = vipsAffine(image, residualx, residualy, o.Interpolator)
		if err != nil {
			return nil, err
		}
	}

	if o.Force {
		o.Crop = false
		o.Embed = false
	}

	image, err = extractOrEmbedImage(image, o)
	if err != nil {
		return nil, err
	}

	debug("Transform: shrink=%v, residual=%v, interpolator=%v",
		shrink, residual, o.Interpolator.String())

	return image, nil
}

func applyEffects(image *C.VipsImage, o Options) (*C.VipsImage, error) {
	var err error

	if o.GaussianBlur.Sigma > 0 || o.GaussianBlur.MinAmpl > 0 {
		image, err = vipsGaussianBlur(image, o.GaussianBlur)
		if err != nil {
			return nil, err
		}
	}

	debug("Effects: gaussSigma=%v, gaussMinAmpl=%v",
		o.GaussianBlur.Sigma, o.GaussianBlur.MinAmpl)

	return image, nil
}

func extractOrEmbedImage(image *C.VipsImage, o Options) (*C.VipsImage, error) {
	var err error = nil
	inWidth := int(image.Xsize)
	inHeight := int(image.Ysize)

	switch {
	case o.Crop:
		width := int(math.Min(float64(inWidth), float64(o.Width)))
		height := int(math.Min(float64(inHeight), float64(o.Height)))
		left, top := calculateCrop(inWidth, inHeight, o.Width, o.Height, o.Gravity)
		left, top = int(math.Max(float64(left), 0)), int(math.Max(float64(top), 0))
		image, err = vipsExtract(image, left, top, width, height)
		break
	case o.Embed:
		left, top := (o.Width-inWidth)/2, (o.Height-inHeight)/2
		image, err = vipsEmbed(image, left, top, o.Width, o.Height, o.Extend)
		break
	case o.Top > 0 || o.Left > 0:
		if o.AreaWidth == 0 {
			o.AreaHeight = o.Width
		}
		if o.AreaHeight == 0 {
			o.AreaHeight = o.Height
		}
		if o.AreaWidth == 0 || o.AreaHeight == 0 {
			return nil, errors.New("Extract area width/height params are required")
		}
		image, err = vipsExtract(image, o.Left, o.Top, o.AreaWidth, o.AreaHeight)
		break
	}

	return image, err
}

func rotateAndFlipImage(image *C.VipsImage, o Options) (*C.VipsImage, error) {
	var err error
	var direction Direction = -1

	if o.NoAutoRotate == false {
		rotation, flip := calculateRotationAndFlip(image, o.Rotate)
		if flip {
			o.Flip = flip
		}
		if rotation > D0 && o.Rotate == 0 {
			o.Rotate = rotation
		}
	}

	if o.Rotate > 0 {
		image, err = vipsRotate(image, getAngle(o.Rotate))
	}

	if o.Flip {
		direction = HORIZONTAL
	} else if o.Flop {
		direction = VERTICAL
	}

	if direction != -1 {
		image, err = vipsFlip(image, direction)
	}

	return image, err
}

func watermakImage(image *C.VipsImage, w Watermark) (*C.VipsImage, error) {
	if w.Text == "" {
		return image, nil
	}

	// Defaults
	if w.Font == "" {
		w.Font = WATERMARK_FONT
	}
	if w.Width == 0 {
		w.Width = int(math.Floor(float64(image.Xsize / 6)))
	}
	if w.DPI == 0 {
		w.DPI = 150
	}
	if w.Margin == 0 {
		w.Margin = w.Width
	}
	if w.Opacity == 0 {
		w.Opacity = 0.25
	} else if w.Opacity > 1 {
		w.Opacity = 1
	}

	image, err := vipsWatermark(image, w)
	if err != nil {
		return nil, err
	}

	return image, nil
}

func zoomImage(image *C.VipsImage, zoom int) (*C.VipsImage, error) {
	if zoom == 0 {
		return image, nil
	}
	return vipsZoom(image, zoom+1)
}

func shrinkImage(image *C.VipsImage, o Options, residual float64, shrink int) (*C.VipsImage, float64, error) {
	// Use vips_shrink with the integral reduction
	image, err := vipsShrink(image, shrink)
	if err != nil {
		return nil, 0, err
	}

	// Recalculate residual float based on dimensions of required vs shrunk images
	residualx := float64(o.Width) / float64(image.Xsize)
	residualy := float64(o.Height) / float64(image.Ysize)

	if o.Crop {
		residual = math.Max(residualx, residualy)
	} else {
		residual = math.Min(residualx, residualy)
	}

	return image, residual, nil
}

func shrinkJpegImage(buf []byte, input *C.VipsImage, factor float64, shrink int) (*C.VipsImage, float64, error) {
	var image *C.VipsImage
	var err error
	shrinkOnLoad := 1

	// Recalculate integral shrink and double residual
	switch {
	case shrink >= 8:
		factor = factor / 8
		shrinkOnLoad = 8
	case shrink >= 4:
		factor = factor / 4
		shrinkOnLoad = 4
	case shrink >= 2:
		factor = factor / 2
		shrinkOnLoad = 2
	}

	// Reload input using shrink-on-load
	if shrinkOnLoad > 1 {
		image, err = vipsShrinkJpeg(buf, input, shrinkOnLoad)
	}

	return image, factor, err
}

func imageCalculations(o *Options, inWidth, inHeight int) float64 {
	factor := 1.0
	xfactor := float64(inWidth) / float64(o.Width)
	yfactor := float64(inHeight) / float64(o.Height)

	switch {
	// Fixed width and height
	case o.Width > 0 && o.Height > 0:
		if o.Crop {
			factor = math.Min(xfactor, yfactor)
		} else {
			factor = math.Max(xfactor, yfactor)
		}
	// Fixed width, auto height
	case o.Width > 0:
		factor = xfactor
		o.Height = int(math.Floor(float64(inHeight) / factor))
	// Fixed height, auto width
	case o.Height > 0:
		factor = yfactor
		o.Width = int(math.Floor(float64(inWidth) / factor))
	// Identity transform
	default:
		o.Width = inWidth
		o.Height = inHeight
		break
	}

	return factor
}

func calculateCrop(inWidth, inHeight, outWidth, outHeight int, gravity Gravity) (int, int) {
	left, top := 0, 0

	switch gravity {
	case NORTH:
		left = (inWidth - outWidth + 1) / 2
	case EAST:
		left = inWidth - outWidth
		top = (inHeight - outHeight + 1) / 2
	case SOUTH:
		left = (inWidth - outWidth + 1) / 2
		top = inHeight - outHeight
	case WEST:
		top = (inHeight - outHeight + 1) / 2
	default:
		left = (inWidth - outWidth + 1) / 2
		top = (inHeight - outHeight + 1) / 2
	}

	return left, top
}

func calculateRotationAndFlip(image *C.VipsImage, angle Angle) (Angle, bool) {
	rotate := D0
	flip := false

	if angle > 0 {
		return rotate, flip
	}

	switch vipsExifOrientation(image) {
	case 6:
		rotate = D90
		break
	case 3:
		rotate = D180
		break
	case 8:
		rotate = D270
		break
	case 2:
		flip = true
		break // flip 1
	case 7:
		flip = true
		rotate = D90
		break // flip 6
	case 4:
		flip = true
		rotate = D180
		break // flip 3
	case 5:
		flip = true
		rotate = D270
		break // flip 8
	}

	return rotate, flip
}

func calculateShrink(factor float64, i Interpolator) int {
	var shrink float64

	// Calculate integral box shrink
	windowSize := vipsWindowSize(i.String())
	if factor >= 2 && windowSize > 3 {
		// Shrink less, affine more with interpolators that use at least 4x4 pixel window, e.g. bicubic
		shrink = float64(math.Floor(factor * 3.0 / windowSize))
	} else {
		shrink = math.Floor(factor)
	}

	return int(math.Max(shrink, 1))
}

func calculateResidual(factor float64, shrink int) float64 {
	return float64(shrink) / factor
}

func getAngle(angle Angle) Angle {
	divisor := angle % 90
	if divisor != 0 {
		angle = angle - divisor
	}
	return Angle(math.Min(float64(angle), 270))
}
