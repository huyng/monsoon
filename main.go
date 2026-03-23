package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var screenshotTempFile string

func captureFrames(display string, intervalMSecs int, width int) {
	for {
		screenshot, err := Capture(display)
		if err != nil {
			fmt.Printf("Failed to capture: %v\n", err)
			continue
		}

		imageFileTemp := screenshotTempFile + ".tmp"
		err = screenshot.SaveJPEG(imageFileTemp, width)
		if err != nil {
			fmt.Printf("Failed to save: %v\n", err)
			continue
		}

		// Atomic rename so the HTTP handler never reads a partially written file.
		os.Rename(imageFileTemp, screenshotTempFile)

		time.Sleep(time.Millisecond * time.Duration(intervalMSecs))
	}
}

func getFrame() []byte {
	data, _ := os.ReadFile(screenshotTempFile)
	return data
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	boundary := "frameBoundary"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)

	for {
		frame := getFrame()

		_, err := fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(frame))
		if err != nil {
			return // client disconnected
		}

		w.Write(frame)
		fmt.Fprintf(w, "\r\n")

		time.Sleep(33 * time.Millisecond)
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

	tempfile, err := os.CreateTemp("", "monsoon-*.jpg")
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(tempfile.Name())
	defer os.Remove(tempfile.Name() + ".tmp")
	defer tempfile.Close()
	screenshotTempFile = tempfile.Name()

	go captureFrames(display, 50, width)

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
