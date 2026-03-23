package main

/*
#cgo LDFLAGS: -lX11
#include <X11/Xlib.h>
#include <X11/Xutil.h>
#include <stdlib.h>
#include <stdint.h>

int get_screen(Display* dpy) {
    return DefaultScreen(dpy);
}

Window get_root(Display* dpy, int screen_num) {
    return RootWindow(dpy, screen_num);
}

// capture_screenshot captures the X11 screen and returns a malloc'd RGB buffer.
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

    uint8_t* data = (uint8_t*)malloc(attrs.width * attrs.height * 3);
    if (!data) {
        XDestroyImage(img);
        return NULL;
    }

    int idx = 0;
    for (int y = 0; y < attrs.height; y++) {
        for (int x = 0; x < attrs.width; x++) {
            unsigned long pixel = XGetPixel(img, x, y);
            data[idx++] = (pixel >> 16) & 0xFF; // R
            data[idx++] = (pixel >>  8) & 0xFF; // G
            data[idx++] =  pixel        & 0xFF; // B
        }
    }

    XDestroyImage(img);
    return data;
}
*/
import "C"
import (
	"fmt"
	"unsafe"
)

// Screenshot holds raw RGB24 pixel data from the X11 display.
type Screenshot struct {
	Width  int
	Height int
	Data   []byte // RGB24: 3 bytes per pixel, row-major
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

	size := int(width) * int(height) * 3
	rgb := make([]byte, size)
	copy(rgb, C.GoBytes(unsafe.Pointer(data), C.int(size)))

	return &Screenshot{
		Width:  int(width),
		Height: int(height),
		Data:   rgb,
	}, nil
}
