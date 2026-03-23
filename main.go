package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// frameBuffer holds the latest JPEG frame in memory and uses a sync.Cond to
// wake HTTP handlers the moment a new frame is ready, avoiding polling delays.
type frameBuffer struct {
	mu   sync.RWMutex
	data []byte
	cond *sync.Cond
}

func newFrameBuffer() *frameBuffer {
	fb := &frameBuffer{}
	fb.cond = sync.NewCond(&fb.mu)
	return fb
}

func (fb *frameBuffer) store(data []byte) {
	fb.mu.Lock()
	fb.data = data
	fb.mu.Unlock()
	fb.cond.Broadcast() // wake all waiting HTTP handlers
}

func (fb *frameBuffer) wait() []byte {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.cond.Wait()
	return fb.data
}

var frameBuf *frameBuffer

func captureFrames(display string, intervalMSecs int, width int) {
	for {
		screenshot, err := Capture(display)
		if err != nil {
			fmt.Printf("Failed to capture: %v\n", err)
			continue
		}

		jpeg, err := screenshot.ToJPEG(width)
		if err != nil {
			fmt.Printf("Failed to encode: %v\n", err)
			continue
		}

		frameBuf.store(jpeg)

		time.Sleep(time.Millisecond * time.Duration(intervalMSecs))
	}
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	boundary := "frameBoundary"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)

	for {
		// Block until a new frame is available rather than polling with a fixed sleep.
		frame := frameBuf.wait()

		_, err := fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(frame))
		if err != nil {
			return // client disconnected
		}

		w.Write(frame)
		fmt.Fprintf(w, "\r\n")
	}
}

func main() {
	var display string
	var port int
	var width int
	flag.StringVar(&display, "d", ":0.0", "X display to capture")
	flag.IntVar(&port, "p", 8080, "HTTP port")
	flag.IntVar(&width, "w", 1280, "Output width in pixels (height scaled proportionally)")
	flag.Parse()

	fmt.Printf("Capturing DISPLAY=%s\n", display)
	os.Setenv("DISPLAY", display)

	frameBuf = newFrameBuffer()

	// Capture at 30 FPS (~33ms), matching the rate clients receive frames.
	go captureFrames(display, 33, width)

	go func() {
		http.HandleFunc("/stream", streamHandler)
		addr := fmt.Sprintf(":%d", port)
		fmt.Printf("Server starting on http://localhost%s/stream\n", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Fatal(err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Println("Received signal, shutting down...")
}
