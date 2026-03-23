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
	flag.StringVar(&display, "d", ":0.0", "X display to capture")
	flag.IntVar(&port, "p", 8080, "HTTP port")
	flag.IntVar(&fps, "r", 30, "Capture frame rate")
	flag.Parse()

	broadcaster = newBroadcaster()
	go broadcaster.run()

	ffmpegCmd, stdout := startFFmpeg(display, fps)

	// Read boxes until we get the init segment (ftyp+moov).
	// The first moof box marks the start of media data.
	initSeg, firstMoof := readInitSegment(stdout)
	broadcaster.initSegment = initSeg
	log.Printf("captured init segment: %d bytes", len(initSeg))

	// Read and broadcast complete moof+mdat fragment pairs.
	go func() {
		// Handle the first moof we already read.
		pendingMoof := firstMoof
		for {
			mdat, err := readBox(stdout)
			if err != nil {
				log.Printf("ffmpeg stdout closed: %v", err)
				return
			}
			fragment := make([]byte, len(pendingMoof)+len(mdat))
			copy(fragment, pendingMoof)
			copy(fragment[len(pendingMoof):], mdat)
			broadcaster.publish <- fragment

			// Read next moof.
			pendingMoof, err = readBox(stdout)
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
