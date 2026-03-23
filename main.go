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
	"syscall"
	"time"
)

const indexHTML = `<!DOCTYPE html>
<html>
<head><title>monsoon</title></head>
<body style="margin:0;background:#000">
<video id="v" autoplay muted controls style="width:100%;height:100vh"></video>
<script>
const ms = new MediaSource();
document.getElementById('v').src = URL.createObjectURL(ms);
ms.addEventListener('sourceopen', () => {
  const sb = ms.addSourceBuffer('video/mp4; codecs="avc1.42E01E"');
  fetch('/stream').then(r => {
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

// Broadcaster fans out fMP4 fragments from ffmpeg to all connected HTTP clients.
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
				default:
					// slow client: drop the chunk rather than blocking
				}
			}
		}
	}
}

// readBox reads one complete MP4 box (header + payload) from r.
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

var broadcaster *Broadcaster

func serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(indexHTML))
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	ch := make(chan []byte, 4)
	broadcaster.register <- ch
	defer func() { broadcaster.unregister <- ch }()

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")

	// Send the fMP4 init segment so the client can start decoding immediately.
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

func startFFmpeg(width, height, fps, scaleWidth int) (*exec.Cmd, io.WriteCloser, io.ReadCloser) {
	fpsStr := fmt.Sprintf("%d", fps)
	// scale=W:-2 maintains aspect ratio; -2 ensures height is divisible by 2 (required for yuv420p)
	scaleFilter := fmt.Sprintf("scale=%d:-2", scaleWidth)
	cmd := exec.Command("ffmpeg",
		"-f", "rawvideo",
		"-pixel_format", "bgr0",
		"-video_size", fmt.Sprintf("%dx%d", width, height),
		"-r", fpsStr,
		"-i", "pipe:0",
		"-vf", scaleFilter,
		"-vcodec", "libx264",
		"-profile:v", "baseline",
		"-level", "3.0",
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

// readInitSegment reads fMP4 boxes until the first moof box, returning the
// accumulated init segment (ftyp+moov) and the first moof box separately.
func readInitSegment(r io.Reader) (initSeg []byte, firstMoof []byte) {
	for {
		box, err := readBox(r)
		if err != nil {
			log.Fatalf("reading fMP4 box: %v", err)
		}
		boxType := string(box[4:8])
		if boxType == "moof" {
			return initSeg, box
		}
		initSeg = append(initSeg, box...)
	}
}

func main() {
	var display string
	var port int
	var fps int
	var scaleWidth int
	flag.StringVar(&display, "d", ":0.0", "X display to capture")
	flag.IntVar(&port, "p", 8080, "HTTP port")
	flag.IntVar(&fps, "r", 30, "Capture frame rate")
	flag.IntVar(&scaleWidth, "w", 1280, "Output width in pixels (height scaled proportionally)")
	flag.Parse()

	broadcaster = newBroadcaster()
	go broadcaster.run()

	// Take one screenshot to get screen dimensions before starting ffmpeg.
	firstShot, err := Capture(display)
	if err != nil {
		log.Fatalf("initial capture failed: %v", err)
	}
	log.Printf("screen size: %dx%d", firstShot.Width, firstShot.Height)

	ffmpegCmd, ffmpegStdin, stdout := startFFmpeg(firstShot.Width, firstShot.Height, fps, scaleWidth)

	// Feed the first frame immediately, then continue at the target rate.
	ffmpegStdin.Write(firstShot.Data)

	// Capture loop: write raw RGB frames to ffmpeg stdin.
	go func() {
		ticker := time.NewTicker(time.Second / time.Duration(fps))
		defer ticker.Stop()
		for range ticker.C {
			shot, err := Capture(display)
			if err != nil {
				log.Printf("capture error: %v", err)
				continue
			}
			if _, err := ffmpegStdin.Write(shot.Data); err != nil {
				log.Printf("ffmpeg stdin closed: %v", err)
				return
			}
		}
	}()

	// Read boxes until we get the init segment (ftyp+moov).
	// The first moof box marks the start of media data.
	initSeg, firstMoof := readInitSegment(stdout)
	broadcaster.initSegment = initSeg
	log.Printf("captured init segment: %d bytes", len(initSeg))

	// Read and broadcast complete moof+mdat fragment pairs.
	go func() {
		pendingMoof := firstMoof
		for {
			box, err := readBox(stdout)
			if err != nil {
				log.Printf("ffmpeg stdout closed: %v", err)
				return
			}
			boxType := string(box[4:8])
			switch boxType {
			case "mdat":
				fragment := make([]byte, len(pendingMoof)+len(box))
				copy(fragment, pendingMoof)
				copy(fragment[len(pendingMoof):], box)
				broadcaster.publish <- fragment
				pendingMoof = nil
			case "moof":
				pendingMoof = box
			default:
				log.Printf("skipping unexpected box type: %s (%d bytes)", boxType, len(box))
			}
		}
	}()

	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/stream", streamHandler)

	go func() {
		addr := fmt.Sprintf(":%d", port)
		log.Printf("serving on http://localhost%s (display=%s, fps=%d)", addr, display, fps)
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
