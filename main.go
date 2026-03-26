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
	fb.cond.Broadcast()
}

func (fb *frameBuffer) wait() []byte {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.cond.Wait()
	return fb.data
}

var frameBuf *frameBuffer

// startCapturePipeline launches two pipelined goroutines:
//  1. capture goroutine: fires at target FPS via ticker, sends raw frames to rawCh
//  2. encode goroutine: resizes and JPEG-encodes each raw frame, stores in frameBuf
//
// Using a ticker (instead of time.Sleep) ensures capture fires at exact wall-clock
// intervals regardless of how long capture takes. The channel buffer of 1 with a
// non-blocking send means the encoder always gets the latest frame; if it's busy,
// the frame is dropped rather than queued.
func startCapturePipeline(capturer *Capturer, fps, width int) {
	rawCh := make(chan *Screenshot, 1)

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
			select {
			case rawCh <- shot:
			default: // encoder busy, drop frame
			}
		}
	}()

	// Encode goroutine
	go func() {
		for shot := range rawCh {
			jpeg, err := shot.ToJPEG(width)
			if err != nil {
				fmt.Printf("Failed to encode: %v\n", err)
				continue
			}
			frameBuf.store(jpeg)
		}
	}()
}

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>monsoon</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

    body {
      background: #0f0f0f;
      color: #e0e0e0;
      font-family: system-ui, sans-serif;
      height: 100dvh;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      gap: 16px;
    }

    h1 {
      font-size: 0.85rem;
      font-weight: 500;
      letter-spacing: 0.1em;
      text-transform: uppercase;
      color: #666;
    }

    .player {
      width: min(100%, 1280px);
      aspect-ratio: 16/9;
      background: #000;
      border-radius: 8px;
      overflow: hidden;
      box-shadow: 0 8px 32px rgba(0,0,0,0.6);
    }

    .player img {
      width: 100%;
      height: 100%;
      object-fit: contain;
      display: block;
    }

    .status {
      font-size: 0.75rem;
      color: #444;
    }

    .dot {
      display: inline-block;
      width: 6px;
      height: 6px;
      border-radius: 50%;
      background: #e53;
      margin-right: 6px;
      animation: pulse 1.5s ease-in-out infinite;
    }

    @keyframes pulse {
      0%, 100% { opacity: 1; }
      50%       { opacity: 0.3; }
    }
  </style>
</head>
<body>
  <h1>monsoon</h1>
  <div class="player">
    <img src="/stream" alt="screen stream">
  </div>
  <p class="status"><span class="dot"></span>live</p>
</body>
</html>`

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
