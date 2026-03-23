package main

/*
#cgo LDFLAGS: -lX11
#include <X11/Xlib.h>
#include <X11/Xutil.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>


int get_screen(Display* dpy) {
    return DefaultScreen(dpy);
}

Window get_root(Display* dpy, int screen_num) {
    return RootWindow(dpy, screen_num);
}

// capture_screenshot captures the X11 screen and returns a malloc'd copy of
// the raw XImage pixel data (BGRX, 4 bytes/pixel).
// width and height are set to the screen dimensions.
// Caller must free the returned pointer.
uint8_t* capture_screenshot(Display* display, int* width, int* height) {
    int screen_num = get_screen(display);
    Window root = get_root(display, screen_num);

    XWindowAttributes attrs;
    XGetWindowAttributes(display, root, &attrs);
    *width  = attrs.width;
    *height = attrs.height;

    XImage* img = XGetImage(display, root, 0, 0, attrs.width, attrs.height, AllPlanes, ZPixmap);
    if (!img) return NULL;

    // Copy raw pixel buffer directly — avoids per-pixel XGetPixel() calls.
    // ZPixmap on Linux is BGRX (4 bytes/pixel); ffmpeg -pixel_format bgr0 matches this.
    size_t size = (size_t)attrs.width * attrs.height * (img->bits_per_pixel / 8);
    uint8_t* data = (uint8_t*)malloc(size);
    if (data) memcpy(data, img->data, size);

    XDestroyImage(img);
    return data;
}
*/
import "C"
import (
	"fmt"
	"image"
	"image/color"
	"unsafe"

	"golang.org/x/image/draw"
)

// Screenshot holds raw BGRX pixel data from the X11 display.
// BGRX is the native ZPixmap format returned by X11 on Linux (4 bytes/pixel,
// B at lowest address, R at +2, X padding at +3).
type Screenshot struct {
	Width  int
	Height int
	Data   []byte // BGRX: 4 bytes/pixel, row-major
}

// bgrxImage wraps a raw BGRX byte slice as an image.Image so it can be
// passed to golang.org/x/image/draw scalers without an extra copy.
type bgrxImage struct {
	data          []byte
	width, height int
}

func (img *bgrxImage) ColorModel() color.Model { return color.NRGBAModel }
func (img *bgrxImage) Bounds() image.Rectangle { return image.Rect(0, 0, img.width, img.height) }
func (img *bgrxImage) At(x, y int) color.Color {
	i := (y*img.width + x) * 4
	return color.NRGBA{R: img.data[i+2], G: img.data[i+1], B: img.data[i+0], A: 255}
}

// ResizeBGRX scales raw BGRX pixel data to the given target width, preserving
// aspect ratio. The output height is rounded up to the nearest even number
// (required by ffmpeg's yuv420p pixel format). Returns resized BGRX bytes
// and the new width and height.
func ResizeBGRX(data []byte, srcW, srcH, dstW int) ([]byte, int, int) {
	dstH := srcH * dstW / srcW
	if dstH%2 != 0 {
		dstH++
	}

	src := &bgrxImage{data: data, width: srcW, height: srcH}
	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	// BiLinear gives good quality at low cost; use draw.CatmullRom for sharper output.
	draw.BiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)

	// Convert image.NRGBA (RGBA order) back to BGRX byte layout.
	out := make([]byte, dstW*dstH*4)
	for i := 0; i < dstW*dstH; i++ {
		out[i*4+0] = dst.Pix[i*4+2] // B
		out[i*4+1] = dst.Pix[i*4+1] // G
		out[i*4+2] = dst.Pix[i*4+0] // R
		out[i*4+3] = 0               // X (padding)
	}
	return out, dstW, dstH
}

// Capture takes a screenshot from the given X display (e.g. ":0.0").
func Capture(display string) (*Screenshot, error) {
	cDisplay := C.CString(display)
	defer C.free(unsafe.Pointer(cDisplay))

	dpy := C.XOpenDisplay(cDisplay)
	if dpy == nil {
		return nil, fmt.Errorf("cannot open display %q", display)
	}
	defer C.XCloseDisplay(dpy)

	var width, height C.int
	data := C.capture_screenshot(dpy, &width, &height)
	if data == nil {
		return nil, fmt.Errorf("capture_screenshot failed")
	}
	defer C.free(unsafe.Pointer(data))

	size := int(width) * int(height) * 4 // BGRX: 4 bytes/pixel
	rgb := make([]byte, size)
	copy(rgb, C.GoBytes(unsafe.Pointer(data), C.int(size)))

	return &Screenshot{
		Width:  int(width),
		Height: int(height),
		Data:   rgb,
	}, nil
}
