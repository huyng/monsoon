package main

/*
#cgo LDFLAGS: -lX11 -lXext
#include <X11/Xlib.h>
#include <X11/Xutil.h>
#include <X11/extensions/XShm.h>
#include <sys/ipc.h>
#include <sys/shm.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>

// CaptureCtx holds a persistent X display connection and MIT-SHM state.
// Reusing the SHM buffer across frames avoids repeated malloc/free and
// eliminates the socket copy that XGetImage incurs — the X server writes
// pixel data directly into shared memory, reducing how long it blocks input.
typedef struct {
    Display*        dpy;
    XImage*         img;       // NULL until first capture
    XShmSegmentInfo shminfo;
    int             shm_width;
    int             shm_height;
} CaptureCtx;

CaptureCtx* new_capture_ctx(const char* display_name) {
    CaptureCtx* c = (CaptureCtx*)calloc(1, sizeof(CaptureCtx));
    if (!c) return NULL;
    c->dpy = XOpenDisplay(display_name);
    if (!c->dpy) { free(c); return NULL; }
    return c;
}

static void shm_cleanup(CaptureCtx* c) {
    if (!c->img) return;
    XShmDetach(c->dpy, &c->shminfo);
    XDestroyImage(c->img);
    shmdt(c->shminfo.shmaddr);
    shmctl(c->shminfo.shmid, IPC_RMID, NULL);
    c->img = NULL;
}

static int shm_init(CaptureCtx* c, int width, int height) {
    shm_cleanup(c);
    int screen = DefaultScreen(c->dpy);
    c->img = XShmCreateImage(c->dpy,
                             DefaultVisual(c->dpy, screen),
                             DefaultDepth(c->dpy, screen),
                             ZPixmap, NULL, &c->shminfo,
                             (unsigned)width, (unsigned)height);
    if (!c->img) return 0;

    c->shminfo.shmid = shmget(IPC_PRIVATE,
                               (size_t)c->img->bytes_per_line * c->img->height,
                               IPC_CREAT | 0600);
    if (c->shminfo.shmid < 0) {
        XDestroyImage(c->img); c->img = NULL; return 0;
    }

    c->shminfo.shmaddr = c->img->data = (char*)shmat(c->shminfo.shmid, NULL, 0);
    c->shminfo.readOnly = False;

    if (!XShmAttach(c->dpy, &c->shminfo)) {
        shmdt(c->shminfo.shmaddr);
        shmctl(c->shminfo.shmid, IPC_RMID, NULL);
        XDestroyImage(c->img); c->img = NULL; return 0;
    }

    c->shm_width  = width;
    c->shm_height = height;
    return 1;
}

void free_capture_ctx(CaptureCtx* c) {
    shm_cleanup(c);
    XCloseDisplay(c->dpy);
    free(c);
}

// capture_screenshot captures the X11 screen via MIT-SHM and returns a
// malloc'd copy of the raw pixel data (BGRX, 4 bytes/pixel).
// The SHM segment is allocated once and reused; it is reinitialised only
// if the screen dimensions change (e.g. resolution switch).
// Caller must free the returned pointer.
uint8_t* capture_screenshot(CaptureCtx* c, int* out_width, int* out_height) {
    int screen = DefaultScreen(c->dpy);
    Window root = RootWindow(c->dpy, screen);

    XWindowAttributes attrs;
    XGetWindowAttributes(c->dpy, root, &attrs);
    *out_width  = attrs.width;
    *out_height = attrs.height;

    if (!c->img || c->shm_width != attrs.width || c->shm_height != attrs.height) {
        if (!shm_init(c, attrs.width, attrs.height)) return NULL;
    }

    if (!XShmGetImage(c->dpy, root, c->img, 0, 0, AllPlanes)) return NULL;

    size_t size = (size_t)attrs.width * attrs.height * (c->img->bits_per_pixel / 8);
    uint8_t* data = (uint8_t*)malloc(size);
    if (data) memcpy(data, c->img->data, size);
    return data;
}

// get_mouse_pos returns the current pointer position relative to the root window.
// Returns 0 on success.
int get_mouse_pos(CaptureCtx* c, int* x, int* y) {
    Window root = RootWindow(c->dpy, DefaultScreen(c->dpy));
    Window root_ret, child_ret;
    int win_x, win_y;
    unsigned int mask;
    Bool ok = XQueryPointer(c->dpy, root, &root_ret, &child_ret,
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

// Capturer holds a persistent X display connection and MIT-SHM state.
// Reusing the connection and SHM buffer across frames avoids the overhead of
// XOpenDisplay/XCloseDisplay and the socket copy that XGetImage incurs.
type Capturer struct {
	ctx *C.CaptureCtx
}

// NewCapturer opens a connection to the given X display and returns a Capturer.
// Call Close() when done.
func NewCapturer(display string) (*Capturer, error) {
	cDisplay := C.CString(display)
	defer C.free(unsafe.Pointer(cDisplay))
	ctx := C.new_capture_ctx(cDisplay)
	if ctx == nil {
		return nil, fmt.Errorf("cannot open display %q", display)
	}
	return &Capturer{ctx: ctx}, nil
}

// Close releases the X display connection and SHM resources.
func (c *Capturer) Close() {
	C.free_capture_ctx(c.ctx)
}

// Capture takes a screenshot using the persistent display connection and SHM buffer.
func (c *Capturer) Capture() (*Screenshot, error) {
	var width, height C.int
	data := C.capture_screenshot(c.ctx, &width, &height)
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
	if C.get_mouse_pos(c.ctx, &x, &y) != 0 {
		return 0, 0, fmt.Errorf("XQueryPointer failed")
	}
	return int(x), int(y), nil
}

// cursorPixels is a 16×16 hardcoded arrow-head cursor pointing upper-left.
// Values: 0 = transparent, 1 = bright yellow fill, 2 = black outline. Hotspot at (0,0).
const cursorW, cursorH = 16, 16

var cursorPixels = [cursorH][cursorW]byte{
	{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	{2, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	{2, 1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	{2, 1, 1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	{2, 1, 1, 1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	{2, 1, 1, 1, 1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	{2, 1, 1, 1, 1, 1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	{2, 1, 1, 1, 1, 1, 1, 2, 0, 0, 0, 0, 0, 0, 0, 0},
	{2, 1, 1, 1, 1, 1, 1, 1, 2, 0, 0, 0, 0, 0, 0, 0},
	{2, 1, 1, 1, 1, 1, 1, 1, 1, 2, 0, 0, 0, 0, 0, 0},
	{2, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 0, 0, 0, 0, 0},
	{2, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 0, 0, 0, 0},
	{2, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 0, 0, 0},
	{2, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 0, 0},
	{2, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 0},
	{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 0},
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
				img.SetRGBA(sx, sy, color.RGBA{R: 255, G: 255, B: 0, A: 255}) // bright yellow
			} else {
				img.SetRGBA(sx, sy, color.RGBA{R: 0, G: 0, B: 0, A: 255}) // black outline
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
		img.Pix[i*4+3] = 255           // A
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
