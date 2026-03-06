package main

/*
#cgo LDFLAGS: -lX11
#include <X11/Xlib.h>
#include <X11/Xutil.h>
#include <stdlib.h>
#include <stdint.h>

// Wrappers for X11 macros
static inline int get_screen(Display* dpy) {
    return DefaultScreen(dpy);
}

static inline Window get_root(Display* dpy, int screen) {
    return RootWindow(dpy, screen);
}

// Capture screenshot and return RGB data
uint8_t* capture_screenshot(Display* display, int* width, int* height) {
    int screen_num = get_screen(display);
    Window root = get_root(display, screen_num);

    XWindowAttributes attrs;
    XGetWindowAttributes(display, root, &attrs);

    XImage* img = XGetImage(display, root, 0, 0,
                            attrs.width, attrs.height,
                            AllPlanes, ZPixmap);
    if (!img) return NULL;

    *width = attrs.width;
    *height = attrs.height;

    // Allocate memory for RGB data (3 bytes per pixel)
    size_t data_size = attrs.width * attrs.height * 3;
    uint8_t* rgb_data = (uint8_t*)malloc(data_size);
    if (!rgb_data) {
        XDestroyImage(img);
        return NULL;
    }

    // Convert XImage to RGB (simplified - assumes 24/32-bit depth)
    for (int y = 0; y < attrs.height; y++) {
        for (int x = 0; x < attrs.width; x++) {
            uint32_t pixel = XGetPixel(img, x, y);
            size_t idx = (y * attrs.width + x) * 3;

            // Extract RGB components (adjust based on your system's masks)
            rgb_data[idx] = (pixel >> 16) & 0xFF;     // Red
            rgb_data[idx + 1] = (pixel >> 8) & 0xFF;  // Green
            rgb_data[idx + 2] = pixel & 0xFF;         // Blue
        }
    }

    XDestroyImage(img);
    return rgb_data;
}
*/
import "C"

import (
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"unsafe"
)

// Screenshot represents a captured image
type Screenshot struct {
	Width  int
	Height int
	Data   []byte // RGB data (3 bytes per pixel)
}

// ToImage converts the screenshot to an image.Image
func (s *Screenshot) ToImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, s.Width, s.Height))

	// Direct access to the pixel buffer
	for y := 0; y < s.Height; y++ {
		srcIdx := y * s.Width * 3
		dstIdx := y * img.Stride

		for x := 0; x < s.Width; x++ {
			img.Pix[dstIdx] = s.Data[srcIdx]     // R
			img.Pix[dstIdx+1] = s.Data[srcIdx+1] // G
			img.Pix[dstIdx+2] = s.Data[srcIdx+2] // B
			img.Pix[dstIdx+3] = 255              // A

			srcIdx += 3
			dstIdx += 4
		}
	}

	return img
}

// SavePNG saves the screenshot as a PNG file
func (s *Screenshot) SavePNG(filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	img := s.ToImage()
	return png.Encode(file, img)
}

func (s *Screenshot) SaveJPEG(filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	img := s.ToImage()
	return jpeg.Encode(file, img, &jpeg.Options{Quality: 40})
}

// Capture takes a screenshot of the entire screen
func Capture() (*Screenshot, error) {
	// Open display
	display := C.XOpenDisplay(nil)
	if display == nil {
		return nil, fmt.Errorf("Failed to open X display")
	}
	defer C.XCloseDisplay(display)

	// Capture screenshot
	var width, height C.int
	rgbData := C.capture_screenshot(display, &width, &height)
	if rgbData == nil {
		return nil, fmt.Errorf("Failed to capture screenshot")
	}
	defer C.free(unsafe.Pointer(rgbData))

	// Copy data to Go slice
	dataSize := int(width) * int(height) * 3
	goData := make([]byte, dataSize)
	copy(goData, C.GoBytes(unsafe.Pointer(rgbData), C.int(dataSize)))

	return &Screenshot{
		Width:  int(width),
		Height: int(height),
		Data:   goData,
	}, nil
}
