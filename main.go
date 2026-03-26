package main

import (
	_ "embed"
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
	fb.cond.Broadcast()
}

func (fb *frameBuffer) wait() []byte {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.cond.Wait()
	return fb.data
}

var frameBuf *frameBuffer

// frame bundles a screenshot with the cursor snapshot taken at the same instant,
// so the cursor position in the encoded image matches the frame content.
type frame struct {
	shot   *Screenshot
	cursor *CursorInfo // nil if XFixes is unavailable
}

// startCapturePipeline launches two pipelined goroutines:
//  1. capture goroutine: fires at target FPS via ticker, sends raw frames to rawCh
//  2. encode goroutine: resizes and JPEG-encodes each raw frame, stores in frameBuf
//
// Using a ticker (instead of time.Sleep) ensures capture fires at exact wall-clock
// intervals regardless of how long capture takes. The channel buffer of 1 with a
// non-blocking send means the encoder always gets the latest frame; if it's busy,
// the frame is dropped rather than queued.
//
// The cursor is captured alongside each screenshot so its position is consistent
// with the frame. XFixes cursor capture is best-effort — failures are silently
// dropped (cursor simply won't appear that frame).
func startCapturePipeline(capturer *Capturer, fps, width int) {
	rawCh := make(chan frame, 1)

	// Capture goroutine
	go func() {
		ticker := time.NewTicker(time.Second / time.Duration(fps))
		defer ticker.Stop()
		for range ticker.C {
			shot, err := capturer.Capture()
			if err != nil {
				fmt.Printf("Failed to capture: %v\n", err)
				continue
			}
			cursor, _ := capturer.GetCursor() // best-effort; nil on failure
			select {
			case rawCh <- frame{shot, cursor}:
			default: // encoder busy, drop frame
			}
		}
	}()

	// Encode goroutine
	go func() {
		for f := range rawCh {
			jpeg, err := f.shot.ToJPEG(width, f.cursor)
			if err != nil {
				fmt.Printf("Failed to encode: %v\n", err)
				continue
			}
			frameBuf.store(jpeg)
		}
	}()
}

//go:embed index.html
var indexHTML string

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(indexHTML))
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	boundary := "frameBoundary"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	flusher := w.(http.Flusher)

	for {
		frame := frameBuf.wait()

		_, err := fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(frame))
		if err != nil {
			return // client disconnected
		}
		w.Write(frame)
		fmt.Fprintf(w, "\r\n")
		flusher.Flush() // send frame to client immediately, no buffering
	}
}

func main() {
	var display string
	var port int
	var width int
	var fps int
	flag.StringVar(&display, "d", ":0.0", "X display to capture")
	flag.IntVar(&port, "p", 8080, "HTTP port")
	flag.IntVar(&width, "w", 1280, "Output width in pixels (height scaled proportionally)")
	flag.IntVar(&fps, "r", 10, "Capture frame rate")
	flag.Parse()

	fmt.Printf("Capturing DISPLAY=%s\n", display)
	os.Setenv("DISPLAY", display)

	capturer, err := NewCapturer(display)
	if err != nil {
		log.Fatalf("failed to open display: %v", err)
	}
	defer capturer.Close()

	frameBuf = newFrameBuffer()

	startCapturePipeline(capturer, fps, width)

	go func() {
		http.HandleFunc("/", indexHandler)
		http.HandleFunc("/stream", streamHandler)
		addr := fmt.Sprintf(":%d", port)
		log.Printf("serving on http://localhost%s", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Fatal(err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Println("Received signal, shutting down...")
}
