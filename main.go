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

func captureFrames(intervalMSecs int) {

	for true {
		screenshot, err := Capture()
		if err != nil {
			fmt.Printf("Failed to capture: %v\n", err)
		}

		imageFileTemp := screenshotTempFile + ".tmp"
		err = screenshot.SaveJPEG(imageFileTemp)
		if err != nil {
			fmt.Printf("Failed to save: %v\n", err)
		}

		// atomic swap to temp file
		os.Rename(imageFileTemp, screenshotTempFile)

		// sleep
		time.Sleep(time.Millisecond * time.Duration(intervalMSecs))
	}
}

func getFrame(frameNum int) []byte {
	// For demonstration, use a placeholder image or actual file
	data, _ := os.ReadFile(screenshotTempFile)
	return data
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	// 1. Set the correct multipart header for MJPEG
	boundary := "frameBoundary"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)

	// 2. Loop to send images
	for i := 0; ; i++ {
		frame := getFrame(i)

		// 3. Write boundary and image headers
		_, err := fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(frame))
		if err != nil {
			return // Client disconnected
		}

		// 4. Send the image data
		w.Write(frame)
		fmt.Fprintf(w, "\r\n")

		// 5. Periodically serve frames (e.g., 20 FPS = 50ms)
		time.Sleep(33 * time.Millisecond)
	}
}

func main() {
	var xDisplay string
	flag.StringVar(&xDisplay, "d", ":1.0", "Display you want to capture from X Window")
	flag.Parse()

	// go routine to capture screenshots
	fmt.Printf("Capturing DISPLAY=%s\n", xDisplay)
	os.Setenv("DISPLAY", xDisplay)

	// Create tempfile to store screenshot image
	tempfile, err := os.CreateTemp("", "monsoon-*.jpg")
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(tempfile.Name())          // clean up
	defer os.Remove(tempfile.Name() + ".tmp") // clean up
	defer tempfile.Close()
	screenshotTempFile = tempfile.Name()
	fmt.Printf("screenshotTempFile = %s\n", screenshotTempFile)

	// Spawn coroutine to periodically capture frames
	go captureFrames(50)

	// Spawn the HTTP server on port 8080
	go func() {
		// Register a handler function for the "/" route
		http.HandleFunc("/stream", streamHandler)
		fmt.Println("Server starting on port 8080...")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatal(err)
		}
	}()

	// Wait for CTRL+C
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	<-signalChan
	fmt.Println("Received signal, shutting down...")
}
