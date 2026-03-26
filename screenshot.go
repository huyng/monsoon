package main

/*
#cgo LDFLAGS: -lX11 -lXfixes
#include <X11/Xlib.h>
#include <X11/Xutil.h>
#include <X11/extensions/Xfixes.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>

int get_screen(Display* dpy) {
    return DefaultScreen(dpy);
}

Window get_root(Display* dpy, int screen_num) {
    return RootWindow(dpy, screen_num);
}

// get_cursor fetches the current cursor image and position via the XFixes
// extension. Pixels are returned as a malloc'd uint32_t[] in ARGB format.
// x,y is the hotspot position on screen; top-left draw origin = (x-xhot, y-yhot).
// Returns 0 on success; caller must free *pixels.
int get_cursor(Display* dpy,
               int* x, int* y, int* xhot, int* yhot,
               int* width, int* height, uint32_t** pixels) {
    XFixesCursorImage* img = XFixesGetCursorImage(dpy);
    if (!img) return -1;
    *x    = img->x;    *y    = img->y;
    *xhot = img->xhot; *yhot = img->yhot;
    *width = img->width; *height = img->height;
    size_t n = (size_t)img->width * img->height;
    *pixels = (uint32_t*)malloc(n * sizeof(uint32_t));
    if (*pixels) {
        for (size_t i = 0; i < n; i++)
            (*pixels)[i] = (uint32_t)img->pixels[i]; // unsigned long → uint32_t
    }
    XFree(img);
    return (*pixels) ? 0 : -1;
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
    // ZPixmap on Linux is BGRX (4 bytes/pixel); see ToImage() for channel swap.
    size_t size = (size_t)attrs.width * attrs.height * (img->bits_per_pixel / 8);
    uint8_t* data = (uint8_t*)malloc(size);
    if (data) memcpy(data, img->data, size);

    XDestroyImage(img);
    return data;
}
*/
import "C"
import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"unsafe"

	"github.com/disintegration/imaging"
)

// Screenshot holds raw BGRX pixel data from the X11 display.
// BGRX is the native ZPixmap format returned by X11 on Linux (4 bytes/pixel,
// B at lowest address, R at +2, X padding at +3).
type Screenshot struct {
	Width  int
	Height int
	Data   []byte // BGRX: 4 bytes/pixel, row-major
}

// Capturer holds a persistent X display connection.
// Reusing the connection across frames avoids the overhead of XOpenDisplay
// and XCloseDisplay on every capture (~5-10ms per call).
type Capturer struct {
	dpy *C.Display
}

// NewCapturer opens a connection to the given X display and returns a Capturer.
// Call Close() when done.
func NewCapturer(display string) (*Capturer, error) {
	cDisplay := C.CString(display)
	defer C.free(unsafe.Pointer(cDisplay))
	dpy := C.XOpenDisplay(cDisplay)
	if dpy == nil {
		return nil, fmt.Errorf("cannot open display %q", display)
	}
	return &Capturer{dpy: dpy}, nil
}

// Close releases the X display connection.
func (c *Capturer) Close() {
	C.XCloseDisplay(c.dpy)
}

// Capture takes a screenshot using the persistent display connection.
func (c *Capturer) Capture() (*Screenshot, error) {
	var width, height C.int
	data := C.capture_screenshot(c.dpy, &width, &height)
	if data == nil {
		return nil, fmt.Errorf("capture_screenshot failed")
	}
	defer C.free(unsafe.Pointer(data))

	size := int(width) * int(height) * 4 // BGRX: 4 bytes/pixel
	buf := C.GoBytes(unsafe.Pointer(data), C.int(size))

	return &Screenshot{
		Width:  int(width),
		Height: int(height),
		Data:   buf,
	}, nil
}

// CursorInfo holds the cursor image and its current screen position.
// Pixels are in ARGB format (8 bits per channel, A in the high byte).
type CursorInfo struct {
	X, Y          int      // hotspot position on screen
	Xhot, Yhot    int      // hotspot offset within the cursor image
	Width, Height int
	Pixels        []uint32 // ARGB, row-major
}

// GetCursor fetches the current cursor image and hotspot position via the
// XFixes extension. XFixes is universally available on Linux X11 servers.
// Returns nil (not an error) when the cursor is temporarily unavailable.
func (c *Capturer) GetCursor() (*CursorInfo, error) {
	var x, y, xhot, yhot, w, h C.int
	var pixels *C.uint32_t
	if C.get_cursor(c.dpy, &x, &y, &xhot, &yhot, &w, &h, &pixels) != 0 {
		return nil, fmt.Errorf("XFixesGetCursorImage failed")
	}
	defer C.free(unsafe.Pointer(pixels))

	n := int(w) * int(h)
	goPixels := make([]uint32, n)
	for i := 0; i < n; i++ {
		goPixels[i] = uint32(*(*C.uint32_t)(unsafe.Pointer(uintptr(unsafe.Pointer(pixels)) + uintptr(i)*4)))
	}
	return &CursorInfo{
		X: int(x), Y: int(y),
		Xhot: int(xhot), Yhot: int(yhot),
		Width: int(w), Height: int(h),
		Pixels: goPixels,
	}, nil
}

// DrawOn alpha-composites the cursor onto img at its current screen position.
// The draw origin is (X-Xhot, Y-Yhot); pixels outside img bounds are clipped.
// Uses the standard Porter-Duff "over" operator for semi-transparent cursors.
func (ci *CursorInfo) DrawOn(img *image.RGBA) {
	startX := ci.X - ci.Xhot
	startY := ci.Y - ci.Yhot
	bounds := img.Bounds()
	for py := 0; py < ci.Height; py++ {
		for px := 0; px < ci.Width; px++ {
			sx, sy := startX+px, startY+py
			if sx < bounds.Min.X || sx >= bounds.Max.X ||
				sy < bounds.Min.Y || sy >= bounds.Max.Y {
				continue
			}
			argb := ci.Pixels[py*ci.Width+px]
			a := uint8(argb >> 24)
			if a == 0 {
				continue
			}
			r := uint8(argb >> 16)
			g := uint8(argb >> 8)
			b := uint8(argb)
			if a == 255 {
				img.SetRGBA(sx, sy, color.RGBA{R: r, G: g, B: b, A: 255})
			} else {
				// Porter-Duff "over": out = src*alpha + dst*(1-alpha)
				dst := img.RGBAAt(sx, sy)
				af := float32(a) / 255
				img.SetRGBA(sx, sy, color.RGBA{
					R: uint8(float32(r)*af + float32(dst.R)*(1-af)),
					G: uint8(float32(g)*af + float32(dst.G)*(1-af)),
					B: uint8(float32(b)*af + float32(dst.B)*(1-af)),
					A: 255,
				})
			}
		}
	}
}

// ToImage converts the raw BGRX pixel data to an image.RGBA,
// swapping the B and R channels to match Go's RGBA layout,
// then composites the cursor on top if cursor is non-nil.
func (s *Screenshot) ToImage(cursor *CursorInfo) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, s.Width, s.Height))
	for i := 0; i < s.Width*s.Height; i++ {
		img.Pix[i*4+0] = s.Data[i*4+2] // R
		img.Pix[i*4+1] = s.Data[i*4+1] // G
		img.Pix[i*4+2] = s.Data[i*4+0] // B
		img.Pix[i*4+3] = 255            // A
	}
	if cursor != nil {
		cursor.DrawOn(img)
	}
	return img
}

// ToJPEG resizes the screenshot to the given width (preserving aspect ratio)
// and returns it encoded as a JPEG byte slice.
// imaging.Linear is used instead of Lanczos — fast enough for screen content
// with no visible quality difference for text/UI.
// Quality 65 balances sharpness and bandwidth for screen sharing.
// The cursor is composited before resize so it scales correctly with the output.
func (s *Screenshot) ToJPEG(width int, cursor *CursorInfo) ([]byte, error) {
	resized := imaging.Resize(s.ToImage(cursor), width, 0, imaging.Linear)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: 65}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
