package main

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/disintegration/imaging"
)

const tileSize = 64
const keyframeInterval = 30 // force full frame every N frames so late clients resync

// streamHub manages connected HTTP streaming clients and broadcasts delta frame messages.
// Each client has a small buffered channel; frames are dropped (not queued) for
// slow clients so the server never blocks on a single lagging connection.
// snapshot holds the most recent keyframe so new clients can initialise their
// canvas immediately on connect.
type streamHub struct {
	mu       sync.Mutex
	clients  map[chan []byte]struct{}
	snapshot []byte
}

func newStreamHub() *streamHub {
	return &streamHub{clients: make(map[chan []byte]struct{})}
}

func (h *streamHub) subscribe() chan []byte {
	ch := make(chan []byte, 4)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	snap := h.snapshot
	h.mu.Unlock()
	if snap != nil {
		ch <- snap
	}
	return ch
}

func (h *streamHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *streamHub) broadcast(msg []byte, isKeyframe bool) {
	h.mu.Lock()
	if isKeyframe {
		h.snapshot = msg
	}
	clients := make([]chan []byte, 0, len(h.clients))
	for ch := range h.clients {
		clients = append(clients, ch)
	}
	h.mu.Unlock()
	for _, ch := range clients {
		select {
		case ch <- msg:
		default: // drop if client can't keep up
		}
	}
}

// dirtyTiles returns the tileSize×tileSize rectangles within curr whose pixels
// differ from the corresponding region in prev. If prev is nil or a different
// size (resolution change), all tiles are returned.
func dirtyTiles(prev, curr *image.NRGBA) []image.Rectangle {
	b := curr.Bounds()
	W, H := b.Dx(), b.Dy()
	tilesX := (W + tileSize - 1) / tileSize
	tilesY := (H + tileSize - 1) / tileSize

	allTiles := func() []image.Rectangle {
		rects := make([]image.Rectangle, 0, tilesX*tilesY)
		for ty := range tilesY {
			for tx := range tilesX {
				x0, y0 := tx*tileSize, ty*tileSize
				rects = append(rects, image.Rect(x0, y0, min(x0+tileSize, W), min(y0+tileSize, H)))
			}
		}
		return rects
	}

	if prev == nil || prev.Bounds().Dx() != W || prev.Bounds().Dy() != H {
		return allTiles()
	}

	var rects []image.Rectangle
	for ty := range tilesY {
		for tx := range tilesX {
			x0, y0 := tx*tileSize, ty*tileSize
			x1, y1 := min(x0+tileSize, W), min(y0+tileSize, H)
			if tileChanged(prev, curr, x0, y0, x1, y1) {
				rects = append(rects, image.Rect(x0, y0, x1, y1))
			}
		}
	}
	return rects
}

// tileChanged returns true if any RGB pixel in the given rectangle differs
// between prev and curr. Alpha is ignored (always 255 for screen content).
func tileChanged(prev, curr *image.NRGBA, x0, y0, x1, y1 int) bool {
	for y := y0; y < y1; y++ {
		off := y*curr.Stride + x0*4
		for x := x0; x < x1; x++ {
			if prev.Pix[off] != curr.Pix[off] ||
				prev.Pix[off+1] != curr.Pix[off+1] ||
				prev.Pix[off+2] != curr.Pix[off+2] {
				return true
			}
			off += 4
		}
	}
	return false
}

// buildDeltaMsg JPEG-encodes each dirty tile and packs them into a single
// binary frame for streaming to clients.
//
// Wire format:
//
//	[1]  uint8  flags       0x01 = keyframe
//	[2]  uint16 width       canvas pixel width
//	[2]  uint16 height      canvas pixel height
//	[2]  uint16 num_tiles
//	Per tile:
//	  [2] uint16 x
//	  [2] uint16 y
//	  [2] uint16 w
//	  [2] uint16 h
//	  [4] uint32 jpeg_len
//	  [N] JPEG bytes
func buildDeltaMsg(tiles []image.Rectangle, img *image.NRGBA, isKeyframe bool) ([]byte, error) {
	type tileJPEG struct {
		r    image.Rectangle
		data []byte
	}
	encoded := make([]tileJPEG, 0, len(tiles))
	for _, r := range tiles {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img.SubImage(r), &jpeg.Options{Quality: 80}); err != nil {
			return nil, err
		}
		encoded = append(encoded, tileJPEG{r, buf.Bytes()})
	}

	b := img.Bounds()
	var out bytes.Buffer
	flags := byte(0)
	if isKeyframe {
		flags = 0x01
	}
	out.WriteByte(flags)
	binary.Write(&out, binary.LittleEndian, uint16(b.Dx()))
	binary.Write(&out, binary.LittleEndian, uint16(b.Dy()))
	binary.Write(&out, binary.LittleEndian, uint16(len(encoded)))
	for _, t := range encoded {
		binary.Write(&out, binary.LittleEndian, uint16(t.r.Min.X))
		binary.Write(&out, binary.LittleEndian, uint16(t.r.Min.Y))
		binary.Write(&out, binary.LittleEndian, uint16(t.r.Dx()))
		binary.Write(&out, binary.LittleEndian, uint16(t.r.Dy()))
		binary.Write(&out, binary.LittleEndian, uint32(len(t.data)))
		out.Write(t.data)
	}
	return out.Bytes(), nil
}

// frame bundles a screenshot with the mouse position sampled at the same instant.
type frame struct {
	shot           *Screenshot
	mouseX, mouseY int
}

var hub *streamHub

// startCapturePipeline launches two pipelined goroutines:
//  1. capture goroutine: fires at target FPS via ticker, sends raw frames to rawCh
//  2. encode goroutine: resizes the frame, computes dirty tiles vs previous frame,
//     builds a binary delta message, and broadcasts it to all streaming clients.
//
// The encode goroutine forces a full keyframe every keyframeInterval frames so
// that late-joining or resyncing clients converge to the correct state quickly.
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
			mx, my, _ := capturer.GetMousePos() // best-effort; (0,0) on failure
			select {
			case rawCh <- frame{shot, mx, my}:
			default: // encoder busy — drop, always show latest
			}
		}
	}()

	// Encode goroutine
	go func() {
		var prevResized *image.NRGBA
		frameCount := 0

		for f := range rawCh {
			scale := float64(width) / float64(f.shot.Width)
			resized := imaging.Resize(f.shot.toRGBA(), width, 0, imaging.Linear)
			drawCursorAt(resized,
				int(float64(f.mouseX)*scale),
				int(float64(f.mouseY)*scale),
				cursorW)

			isKeyframe := prevResized == nil || frameCount%keyframeInterval == 0
			var dirty []image.Rectangle
			if isKeyframe {
				dirty = dirtyTiles(nil, resized)
			} else {
				dirty = dirtyTiles(prevResized, resized)
			}
			frameCount++

			if len(dirty) == 0 {
				prevResized = resized
				continue
			}

			msg, err := buildDeltaMsg(dirty, resized, isKeyframe)
			if err != nil {
				fmt.Printf("Failed to encode delta: %v\n", err)
				continue
			}
			hub.broadcast(msg, isKeyframe)
			prevResized = resized
		}
	}()
}

// streamHandler streams binary delta frames over a plain HTTP chunked response.
// Each frame is prefixed with its 4-byte little-endian length so the client
// can reassemble message boundaries from the byte stream.
func streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx proxy buffering
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch := hub.subscribe()
	defer hub.unsubscribe(ch)

	var lenBuf [4]byte
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(msg)))
			if _, err := w.Write(lenBuf[:]); err != nil {
				return
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

//go:embed index.html
var indexHTML string

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(indexHTML))
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

	hub = newStreamHub()
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
