package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
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
      if (sb.updating) {
        sb.addEventListener('updateend', () => sb.appendBuffer(value), {once: true});
      } else {
        sb.appendBuffer(value);
      }
      pump();
    });
    pump();
  });
});
</script>
</body>
</html>`

// Broadcaster fans out fMP4 chunks from ffmpeg to all connected HTTP clients.
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

func startFFmpeg(display string, fps int) (*exec.Cmd, io.ReadCloser) {
	fpsStr := fmt.Sprintf("%d", fps)
	cmd := exec.Command("ffmpeg",
		"-f", "x11grab",
		"-r", fpsStr,
		"-i", display,
		"-vcodec", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-pix_fmt", "yuv420p",
		"-g", fpsStr,
		"-f", "mp4",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"pipe:1",
	)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("ffmpeg stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("ffmpeg start: %v", err)
	}
	return cmd, stdout
}

func main() {
	var display string
	var port int
	var fps int
	flag.StringVar(&display, "d", ":0.0", "X display to capture")
	flag.IntVar(&port, "p", 8080, "HTTP port")
	flag.IntVar(&fps, "r", 30, "Capture frame rate")
	flag.Parse()

	broadcaster = newBroadcaster()
	go broadcaster.run()

	ffmpegCmd, stdout := startFFmpeg(display, fps)

	// Read the fMP4 init segment (first chunk before any moof boxes).
	// With empty_moov, ffmpeg flushes the init segment as the first write.
	initBuf := make([]byte, 32*1024)
	n, err := stdout.Read(initBuf)
	if err != nil {
		log.Fatalf("reading ffmpeg init segment: %v", err)
	}
	broadcaster.initSegment = make([]byte, n)
	copy(broadcaster.initSegment, initBuf[:n])
	log.Printf("captured init segment: %d bytes", n)

	// Continuously read ffmpeg output and broadcast to clients.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				broadcaster.publish <- chunk
			}
			if err != nil {
				log.Printf("ffmpeg stdout closed: %v", err)
				return
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
