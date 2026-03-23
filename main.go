package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// frameBuffer: in-memory frame store with signal-based delivery.
// Stores the latest []byte and wakes all waiting goroutines on each update.
// ---------------------------------------------------------------------------

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

// frameBuf holds the latest JPEG frame (feeds /stream MJPEG endpoint).
// frameBufRaw holds the latest raw BGRX frame (feeds ffmpeg stdin for /video).
var (
	frameBuf    *frameBuffer
	frameBufRaw *frameBuffer
)

// ---------------------------------------------------------------------------
// Capture loop: populates both frame buffers on every frame.
// ---------------------------------------------------------------------------

func captureFrames(display string, intervalMSecs int, width int) {
	for {
		screenshot, err := Capture(display)
		if err != nil {
			fmt.Printf("Failed to capture: %v\n", err)
			continue
		}

		// Store raw BGRX for ffmpeg encoding (/video endpoint).
		frameBufRaw.store(screenshot.Data)

		// Store JPEG for MJPEG streaming (/stream endpoint).
		jpeg, err := screenshot.ToJPEG(width)
		if err != nil {
			fmt.Printf("Failed to encode: %v\n", err)
			continue
		}
		frameBuf.store(jpeg)

		time.Sleep(time.Millisecond * time.Duration(intervalMSecs))
	}
}

// ---------------------------------------------------------------------------
// MJPEG streaming (/stream)
// ---------------------------------------------------------------------------

func streamHandler(w http.ResponseWriter, r *http.Request) {
	boundary := "frameBoundary"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)

	for {
		frame := frameBuf.wait()

		_, err := fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(frame))
		if err != nil {
			return // client disconnected
		}
		w.Write(frame)
		fmt.Fprintf(w, "\r\n")
	}
}

// ---------------------------------------------------------------------------
// fMP4 / H.264 streaming (/video)
// ---------------------------------------------------------------------------

// Broadcaster fans out fMP4 fragments to all connected /video clients.
type Broadcaster struct {
	initSegment []byte
	clients     map[chan []byte]struct{}
	register    chan chan []byte
	unregister  chan chan []byte
	publish     chan []byte
}

func newBroadcaster() *Broadcaster {
	return &Broadcaster{
		clients:    make(map[chan []byte]struct{}),
		register:   make(chan chan []byte),
		unregister: make(chan chan []byte),
		publish:    make(chan []byte, 8),
	}
}

func (b *Broadcaster) run() {
	for {
		select {
		case ch := <-b.register:
			b.clients[ch] = struct{}{}
		case ch := <-b.unregister:
			delete(b.clients, ch)
			close(ch)
		case chunk := <-b.publish:
			for ch := range b.clients {
				select {
				case ch <- chunk:
				default: // slow client: drop rather than block
				}
			}
		}
	}
}

// readBox reads one complete MP4 box (4-byte size + 4-byte type + payload).
func readBox(r io.Reader) ([]byte, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:4])
	if size < 8 {
		return nil, fmt.Errorf("invalid box size %d", size)
	}
	box := make([]byte, size)
	copy(box, header)
	if _, err := io.ReadFull(r, box[8:]); err != nil {
		return nil, err
	}
	return box, nil
}

// readInitSegment reads fMP4 boxes until the first moof box.
// Everything before it (ftyp+moov) is the init segment new clients need.
func readInitSegment(r io.Reader) (initSeg []byte, firstMoof []byte) {
	for {
		box, err := readBox(r)
		if err != nil {
			log.Fatalf("reading fMP4 init segment: %v", err)
		}
		if string(box[4:8]) == "moof" {
			return initSeg, box
		}
		initSeg = append(initSeg, box...)
	}
}

func startFFmpeg(srcWidth, srcHeight, dstWidth, fps int) (*exec.Cmd, io.WriteCloser, io.ReadCloser) {
	fpsStr := fmt.Sprintf("%d", fps)
	cmd := exec.Command("ffmpeg",
		"-f", "rawvideo",
		"-pixel_format", "bgr0",
		"-video_size", fmt.Sprintf("%dx%d", srcWidth, srcHeight),
		"-r", fpsStr,
		"-i", "pipe:0",
		// scale=W:-2 maintains aspect ratio; -2 rounds height to even (required for yuv420p)
		"-vf", fmt.Sprintf("scale=%d:-2", dstWidth),
		"-vcodec", "libx264",
		"-profile:v", "baseline",
		// Level 4.1 supports 1080p@30fps and 720p@60fps.
		// Must match the avc1.42E0LL codec string in the HTML (LL=29 hex=4.1).
		// Lower levels (e.g. 3.0) silently break the stream at non-default FPS.
		"-level", "4.1",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-pix_fmt", "yuv420p",
		"-g", fpsStr,
		"-f", "mp4",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"pipe:1",
	)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalf("ffmpeg stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("ffmpeg stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("ffmpeg start: %v", err)
	}
	return cmd, stdin, stdout
}

var broadcaster *Broadcaster

func videoHandler(w http.ResponseWriter, r *http.Request) {
	ch := make(chan []byte, 4)
	broadcaster.register <- ch
	defer func() { broadcaster.unregister <- ch }()

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")

	if _, err := w.Write(broadcaster.initSegment); err != nil {
		return
	}
	w.(http.Flusher).Flush()

	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			return
		}
	}
}

const indexHTML = `<!DOCTYPE html>
<html>
<head><title>monsoon</title></head>
<body style="margin:0;background:#000">
<video id="v" autoplay muted controls style="width:100%;height:100vh"></video>
<script>
const ms = new MediaSource();
document.getElementById('v').src = URL.createObjectURL(ms);
ms.addEventListener('sourceopen', () => {
  // codec string format: avc1.PPCCLL
  //   PP = profile: 42 = Baseline
  //   CC = constraint flags: E0 = constrained baseline
  //   LL = level in hex: 29 = 4.1 (supports 1080p@30fps, 720p@60fps)
  // This MUST match the -profile:v and -level passed to ffmpeg.
  // Using too low a level (e.g. 3.0 = 0x1E) causes the browser to reject
  // the stream at higher frame rates or resolutions.
  const sb = ms.addSourceBuffer('video/mp4; codecs="avc1.42E029"');
  fetch('/video').then(r => {
    const reader = r.body.getReader();
    const pump = () => reader.read().then(({done, value}) => {
      if (done) return;
      const append = () => {
        if (sb.updating) {
          sb.addEventListener('updateend', append, {once: true});
        } else {
          sb.appendBuffer(value);
          pump();
        }
      };
      append();
    });
    pump();
  });
});
</script>
</body>
</html>`

func serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(indexHTML))
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	var display string
	var port int
	var width int
	var fps int
	flag.StringVar(&display, "d", ":0.0", "X display to capture")
	flag.IntVar(&port, "p", 8080, "HTTP port")
	flag.IntVar(&width, "w", 1280, "Output width in pixels (height scaled proportionally)")
	flag.IntVar(&fps, "r", 30, "Capture frame rate")
	flag.Parse()

	fmt.Printf("Capturing DISPLAY=%s\n", display)
	os.Setenv("DISPLAY", display)

	frameBuf = newFrameBuffer()
	frameBufRaw = newFrameBuffer()

	// Initial capture to get screen dimensions for ffmpeg.
	firstShot, err := Capture(display)
	if err != nil {
		log.Fatalf("initial capture failed: %v", err)
	}
	log.Printf("screen size: %dx%d", firstShot.Width, firstShot.Height)
	frameBufRaw.store(firstShot.Data)

	// Start capture loop (populates both frameBuf and frameBufRaw).
	go captureFrames(display, 1000/fps, width)

	// ---------------------------------------------------------------------------
	// fMP4 pipeline: raw frames → ffmpeg → broadcaster → /video clients
	// A drop channel (size 1) decouples capture from encoding so keyframe spikes
	// don't stall the capture loop.
	// ---------------------------------------------------------------------------
	broadcaster = newBroadcaster()
	go broadcaster.run()

	ffmpegCmd, ffmpegStdin, ffmpegStdout := startFFmpeg(firstShot.Width, firstShot.Height, width, fps)

	rawCh := make(chan []byte, 1)

	// Pull goroutine: waits for new raw frames, drops if ffmpeg is busy.
	go func() {
		for {
			data := frameBufRaw.wait()
			select {
			case rawCh <- data:
			default: // ffmpeg busy (e.g. encoding keyframe): drop this frame
			}
		}
	}()

	// Write goroutine: feeds ffmpeg stdin; allowed to block without affecting capture.
	go func() {
		for data := range rawCh {
			if _, err := ffmpegStdin.Write(data); err != nil {
				log.Printf("ffmpeg stdin closed: %v", err)
				return
			}
		}
	}()

	// Read fMP4 init segment (ftyp+moov) then broadcast moof+mdat fragment pairs.
	initSeg, firstMoof := readInitSegment(ffmpegStdout)
	broadcaster.initSegment = initSeg
	log.Printf("fMP4 init segment: %d bytes", len(initSeg))

	go func() {
		pendingMoof := firstMoof
		for {
			box, err := readBox(ffmpegStdout)
			if err != nil {
				log.Printf("ffmpeg stdout closed: %v", err)
				return
			}
			switch string(box[4:8]) {
			case "mdat":
				fragment := make([]byte, len(pendingMoof)+len(box))
				copy(fragment, pendingMoof)
				copy(fragment[len(pendingMoof):], box)
				broadcaster.publish <- fragment
				pendingMoof = nil
			case "moof":
				pendingMoof = box
			default:
				log.Printf("skipping unexpected box: %s (%d bytes)", string(box[4:8]), len(box))
			}
		}
	}()

	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/stream", streamHandler)
	http.HandleFunc("/video", videoHandler)

	go func() {
		addr := fmt.Sprintf(":%d", port)
		log.Printf("serving on http://localhost%s  (MJPEG: /stream  H.264: /)", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Fatal(err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("shutting down...")
	ffmpegCmd.Process.Kill()
}
