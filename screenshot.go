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
    // ZPixmap on Linux is BGRX (4 bytes/pixel); see ToImage() for channel swap.
    size_t size = (size_t)attrs.width * attrs.height * (img->bits_per_pixel / 8);
    uint8_t* data = (uint8_t*)malloc(size);
    if (data) memcpy(data, img->data, size);

    XDestroyImage(img);
    return data;
}

// get_mouse_pos returns the current pointer position relative to the root window.
// Returns 0 on success.
int get_mouse_pos(Display* dpy, int* x, int* y) {
    Window root = RootWindow(dpy, DefaultScreen(dpy));
    Window root_ret, child_ret;
    int win_x, win_y;
    unsigned int mask;
    Bool ok = XQueryPointer(dpy, root, &root_ret, &child_ret,
                            x, y, &win_x, &win_y, &mask);
    return ok ? 0 : -1;
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

// GetMousePos returns the current pointer position on the root window
// using XQueryPointer — no XFixes extension required.
func (c *Capturer) GetMousePos() (int, int, error) {
	var x, y C.int
	if C.get_mouse_pos(c.dpy, &x, &y) != 0 {
		return 0, 0, fmt.Errorf("XQueryPointer failed")
	}
	return int(x), int(y), nil
}

// cursorPixels is a 10×10 hardcoded arrow-head cursor pointing upper-left.
// Values: 0 = transparent, 1 = white fill, 2 = black outline. Hotspot at (0,0).
const cursorW, cursorH = 10, 10

var cursorPixels = [cursorH][cursorW]byte{
	{2, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	{2, 2, 0, 0, 0, 0, 0, 0, 0, 0},
	{2, 1, 2, 0, 0, 0, 0, 0, 0, 0},
	{2, 1, 1, 2, 0, 0, 0, 0, 0, 0},
	{2, 1, 1, 1, 2, 0, 0, 0, 0, 0},
	{2, 1, 1, 1, 1, 2, 0, 0, 0, 0},
	{2, 1, 1, 1, 1, 1, 2, 0, 0, 0},
	{2, 1, 1, 1, 1, 1, 1, 2, 0, 0},
	{2, 1, 1, 1, 1, 1, 1, 1, 2, 0},
	{2, 2, 2, 2, 2, 2, 2, 2, 2, 0},
}

// drawCursorAt paints the hardcoded arrow cursor onto img with its tip at (x, y).
// Pixels that fall outside the image bounds are clipped.
func drawCursorAt(img *image.RGBA, x, y int) {
	bounds := img.Bounds()
	for py := 0; py < cursorH; py++ {
		for px := 0; px < cursorW; px++ {
			v := cursorPixels[py][px]
			if v == 0 {
				continue
			}
			sx, sy := x+px, y+py
			if sx < bounds.Min.X || sx >= bounds.Max.X ||
				sy < bounds.Min.Y || sy >= bounds.Max.Y {
				continue
			}
			if v == 1 {
				img.SetRGBA(sx, sy, color.RGBA{R: 255, G: 255, B: 255, A: 255})
			} else {
				img.SetRGBA(sx, sy, color.RGBA{R: 0, G: 0, B: 0, A: 255})
			}
		}
	}
}

// ToImage converts the raw BGRX pixel data to an image.RGBA,
// swapping the B and R channels to match Go's RGBA layout,
// then draws the cursor at the given screen position.
func (s *Screenshot) ToImage(mouseX, mouseY int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, s.Width, s.Height))
	for i := 0; i < s.Width*s.Height; i++ {
		img.Pix[i*4+0] = s.Data[i*4+2] // R
		img.Pix[i*4+1] = s.Data[i*4+1] // G
		img.Pix[i*4+2] = s.Data[i*4+0] // B
		img.Pix[i*4+3] = 255            // A
	}
	drawCursorAt(img, mouseX, mouseY)
	return img
}

// ToJPEG resizes the screenshot to the given width (preserving aspect ratio)
// and returns it encoded as a JPEG byte slice.
// imaging.Linear is used instead of Lanczos — fast enough for screen content
// with no visible quality difference for text/UI.
// Quality 65 balances sharpness and bandwidth for screen sharing.
// The cursor is drawn before resize so it scales correctly with the output.
func (s *Screenshot) ToJPEG(width, mouseX, mouseY int) ([]byte, error) {
	resized := imaging.Resize(s.ToImage(mouseX, mouseY), width, 0, imaging.Linear)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, resized, &jpeg.Options{Quality: 65}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
