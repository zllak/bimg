package bimg

/*
#cgo pkg-config: vips
#include "vips.h"
*/
import "C"

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

const HasMagickSupport = int(C.VIPS_MAGICK_SUPPORT) == 1

const (
	maxCacheMem  = 100 * 1024 * 1024
	maxCacheSize = 500
)

var (
	m           sync.Mutex
	initialized bool
)

type VipsMemoryInfo struct {
	Memory          int64
	MemoryHighwater int64
	Allocations     int64
}

type vipsSaveOptions struct {
	Quality        int
	Compression    int
	Type           ImageType
	Interlace      bool
	NoProfile      bool
	Interpretation Interpretation
}

type vipsWatermarkOptions struct {
	Width       C.int
	DPI         C.int
	Margin      C.int
	NoReplicate C.int
	Opacity     C.float
	Background  [3]C.double
}

type vipsWatermarkTextOptions struct {
	Text *C.char
	Font *C.char
}

func init() {
	Initialize()
}

// Explicit thread-safe start of libvips.
// Only call this function if you've previously shutdown libvips
func Initialize() {
	if C.VIPS_MAJOR_VERSION <= 7 && C.VIPS_MINOR_VERSION < 40 {
		panic("unsupported libvips version!")
	}

	m.Lock()
	runtime.LockOSThread()
	defer m.Unlock()
	defer runtime.UnlockOSThread()

	err := C.vips_init(C.CString("bimg"))
	if err != 0 {
		panic("unable to start vips!")
	}

	// Set libvips cache params
	C.vips_cache_set_max_mem(maxCacheMem)
	C.vips_cache_set_max(maxCacheSize)

	// Define a custom thread concurrency limit in libvips (this may generate thread-unsafe issues)
	// See: https://github.com/jcupitt/libvips/issues/261#issuecomment-92850414
	if os.Getenv("VIPS_CONCURRENCY") == "" {
		C.vips_concurrency_set(1)
	}

	// Enable libvips cache tracing
	if os.Getenv("VIPS_TRACE") != "" {
		C.vips_enable_cache_set_trace()
	}

	initialized = true
}

// Thread-safe function to shutdown libvips.
// You can call this to drop caches as well.
// If libvips was already initialized, the function is no-op
func Shutdown() {
	m.Lock()
	defer m.Unlock()

	if initialized {
		C.vips_shutdown()
		initialized = false
	}
}

// Output to stdout vips collected data. Useful for debugging
func VipsDebugInfo() {
	C.im__print_all()
}

// Get memory info stats from vips (cache size, memory allocs...)
func VipsMemory() VipsMemoryInfo {
	return VipsMemoryInfo{
		Memory:          int64(C.vips_tracked_get_mem()),
		MemoryHighwater: int64(C.vips_tracked_get_mem_highwater()),
		Allocations:     int64(C.vips_tracked_get_allocs()),
	}
}

func vipsExifOrientation(image *C.VipsImage) int {
	return int(C.vips_exif_orientation(image))
}

func vipsHasAlpha(image *C.VipsImage) bool {
	return int(C.has_alpha_channel(image)) > 0
}

func vipsHasProfile(image *C.VipsImage) bool {
	return int(C.has_profile_embed(image)) > 0
}

func vipsWindowSize(name string) float64 {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	return float64(C.interpolator_window_size(cname))
}

func vipsSpace(image *C.VipsImage) string {
	return C.GoString(C.vips_enum_nick_bridge(image))
}

func vipsRotate(image *C.VipsImage, angle Angle) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_rotate(image, &out, C.int(angle))
	if err != 0 {
		return nil, catchVipsError()
	}

	return out, nil
}

func vipsFlip(image *C.VipsImage, direction Direction) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_flip_bridge(image, &out, C.int(direction))
	if err != 0 {
		return nil, catchVipsError()
	}

	return out, nil
}

func vipsZoom(image *C.VipsImage, zoom int) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_zoom_bridge(image, &out, C.int(zoom), C.int(zoom))
	if err != 0 {
		return nil, catchVipsError()
	}

	return out, nil
}

func vipsWatermark(image *C.VipsImage, w Watermark) (*C.VipsImage, error) {
	var out *C.VipsImage

	// Defaults
	noReplicate := 0
	if w.NoReplicate {
		noReplicate = 1
	}

	text := C.CString(w.Text)
	font := C.CString(w.Font)
	background := [3]C.double{C.double(w.Background.R), C.double(w.Background.G), C.double(w.Background.B)}

	textOpts := vipsWatermarkTextOptions{text, font}
	opts := vipsWatermarkOptions{C.int(w.Width), C.int(w.DPI), C.int(w.Margin), C.int(noReplicate), C.float(w.Opacity), background}

	defer C.free(unsafe.Pointer(text))
	defer C.free(unsafe.Pointer(font))

	err := C.vips_watermark(image, &out, (*C.WatermarkTextOptions)(unsafe.Pointer(&textOpts)), (*C.WatermarkOptions)(unsafe.Pointer(&opts)))
	if err != 0 {
		return nil, catchVipsError()
	}

	return out, nil
}

func vipsRead(buf []byte) (*C.VipsImage, ImageType, error) {
	var image *C.VipsImage
	imageType := vipsImageType(buf)

	if imageType == UNKNOWN {
		return nil, UNKNOWN, errors.New("Unsupported image format")
	}

	length := C.size_t(len(buf))
	imageBuf := unsafe.Pointer(&buf[0])

	err := C.vips_init_image(imageBuf, length, C.int(imageType), &image)
	if err != 0 {
		return nil, UNKNOWN, catchVipsError()
	}

	return image, imageType, nil
}

func vipsColourspaceIsSupportedBuffer(buf []byte) (bool, error) {
	image, _, err := vipsRead(buf)
	if err != nil {
		return false, err
	}
	C.g_object_unref(C.gpointer(image))
	return vipsColourspaceIsSupported(image), nil
}

func vipsColourspaceIsSupported(image *C.VipsImage) bool {
	return int(C.vips_colourspace_issupported_bridge(image)) == 1
}

func vipsInterpretationBuffer(buf []byte) (Interpretation, error) {
	image, _, err := vipsRead(buf)
	if err != nil {
		return INTERPRETATION_ERROR, err
	}
	C.g_object_unref(C.gpointer(image))
	return vipsInterpretation(image), nil
}

func vipsInterpretation(image *C.VipsImage) Interpretation {
	return Interpretation(C.vips_image_guess_interpretation_bridge(image))
}

func vipsPreSave(image *C.VipsImage, o *vipsSaveOptions) (*C.VipsImage, error) {
	// Remove ICC profile metadata
	if o.NoProfile {
		C.remove_profile(image)
	}

	// Use a default interpretation and cast it to C type
	if o.Interpretation == 0 {
		o.Interpretation = INTERPRETATION_sRGB
	}
	interpretation := C.VipsInterpretation(o.Interpretation)

	// Apply the proper colour space
	var outImage *C.VipsImage
	if vipsColourspaceIsSupported(image) {
		err := int(C.vips_colourspace_bridge(image, &outImage, interpretation))
		if err != 0 {
			return nil, catchVipsError()
		}
		C.g_object_unref(C.gpointer(image))
		image = outImage
	}

	return image, nil
}

func vipsSave(image *C.VipsImage, o vipsSaveOptions) ([]byte, error) {
	defer C.g_object_unref(C.gpointer(image))

	image, err := vipsPreSave(image, &o)
	if err != nil {
		return nil, err
	}

	length := C.size_t(0)
	saveErr := C.int(0)
	interlace := C.int(boolToInt(o.Interlace))
	quality := C.int(o.Quality)

	var ptr unsafe.Pointer
	switch o.Type {
	case WEBP:
		saveErr = C.vips_webpsave_bridge(image, &ptr, &length, 1, quality)
		break
	case PNG:
		saveErr = C.vips_pngsave_bridge(image, &ptr, &length, 1, C.int(o.Compression), quality, interlace)
		break
	default:
		saveErr = C.vips_jpegsave_bridge(image, &ptr, &length, 1, quality, interlace)
		break
	}

	if int(saveErr) != 0 {
		return nil, catchVipsError()
	}

	buf := C.GoBytes(ptr, C.int(length))

	// Clean up
	C.g_free(C.gpointer(ptr))
	C.vips_error_clear()

	return buf, nil
}

func vipsExtract(image *C.VipsImage, left, top, width, height int) (*C.VipsImage, error) {
	var buf *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	if width > MAX_SIZE || height > MAX_SIZE {
		return nil, errors.New("Maximum image size exceeded")
	}

	err := C.vips_extract_area_bridge(image, &buf, C.int(left), C.int(top), C.int(width), C.int(height))
	if err != 0 {
		return nil, catchVipsError()
	}

	return buf, nil
}

func vipsShrinkJpeg(buf []byte, input *C.VipsImage, shrink int) (*C.VipsImage, error) {
	var image *C.VipsImage
	defer C.g_object_unref(C.gpointer(input))

	err := C.vips_jpegload_buffer_shrink(unsafe.Pointer(&buf[0]), C.size_t(len(buf)), &image, C.int(shrink))
	if err != 0 {
		return nil, catchVipsError()
	}

	return image, nil
}

func vipsShrink(input *C.VipsImage, shrink int) (*C.VipsImage, error) {
	var image *C.VipsImage
	defer C.g_object_unref(C.gpointer(input))

	err := C.vips_shrink_bridge(input, &image, C.double(float64(shrink)), C.double(float64(shrink)))
	if err != 0 {
		return nil, catchVipsError()
	}

	return image, nil
}

func vipsEmbed(input *C.VipsImage, left, top, width, height, extend int) (*C.VipsImage, error) {
	var image *C.VipsImage
	defer C.g_object_unref(C.gpointer(input))

	err := C.vips_embed_bridge(input, &image, C.int(left), C.int(top), C.int(width), C.int(height), C.int(extend))
	if err != 0 {
		return nil, catchVipsError()
	}

	return image, nil
}

func vipsAffine(input *C.VipsImage, residualx, residualy float64, i Interpolator) (*C.VipsImage, error) {
	var image *C.VipsImage
	cstring := C.CString(i.String())
	interpolator := C.vips_interpolate_new(cstring)

	defer C.free(unsafe.Pointer(cstring))
	defer C.g_object_unref(C.gpointer(input))
	defer C.g_object_unref(C.gpointer(interpolator))

	err := C.vips_affine_interpolator(input, &image, C.double(residualx), 0, 0, C.double(residualy), interpolator)
	if err != 0 {
		return nil, catchVipsError()
	}

	return image, nil
}

func vipsImageType(bytes []byte) ImageType {
	if len(bytes) == 0 {
		return UNKNOWN
	}

	if bytes[0] == 0x89 && bytes[1] == 0x50 && bytes[2] == 0x4E && bytes[3] == 0x47 {
		return PNG
	}
	if bytes[0] == 0xFF && bytes[1] == 0xD8 && bytes[2] == 0xFF {
		return JPEG
	}
	if bytes[8] == 0x57 && bytes[9] == 0x45 && bytes[10] == 0x42 && bytes[11] == 0x50 {
		return WEBP
	}
	if (bytes[0] == 0x49 && bytes[1] == 0x49 && bytes[2] == 0x2A && bytes[3] == 0x0) ||
		(bytes[0] == 0x4D && bytes[1] == 0x4D && bytes[2] == 0x0 && bytes[3] == 0x2A) {
		return TIFF
	}
	if HasMagickSupport && strings.HasSuffix(readImageType(bytes), "MagickBuffer") {
		return MAGICK
	}

	return UNKNOWN
}

func readImageType(buf []byte) string {
	length := C.size_t(len(buf))
	imageBuf := unsafe.Pointer(&buf[0])
	load := C.vips_foreign_find_load_buffer(imageBuf, length)
	defer C.free(imageBuf)
	return C.GoString(load)
}

func catchVipsError() error {
	s := C.GoString(C.vips_error_buffer())
	C.vips_error_clear()
	C.vips_thread_shutdown()
	return errors.New(s)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func vipsGaussianBlur(image *C.VipsImage, o GaussianBlur) (*C.VipsImage, error) {
	var out *C.VipsImage
	defer C.g_object_unref(C.gpointer(image))

	err := C.vips_gaussblur_bridge(image, &out, C.double(o.Sigma), C.double(o.MinAmpl))
	if err != 0 {
		return nil, catchVipsError()
	}
	return out, nil
}
