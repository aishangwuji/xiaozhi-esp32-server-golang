// Command pr281_e2e sends a short WAV through the live WebSocket voice pipeline.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/hraban/opus.v2"
	"xiaozhi-esp32-server-golang/internal/util"
)

type serverEvent struct {
	Type  string `json:"type"`
	State string `json:"state"`
	Text  string `json:"text"`
	Bytes int
	Decoded bool
	Signal bool
	Err   error
}

func sendJSON(conn *websocket.Conn, data any) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, encoded)
}

func run(url, wavPath string, timeout time.Duration) error {
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		return err
	}
	frames, err := util.WavToOpus(wav, 16000, 1, 0)
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return errors.New("empty WAV")
	}
	encoder, err := opus.NewEncoder(16000, 1, opus.AppVoIP)
	if err != nil {
		return err
	}
	silenceBuffer := make([]byte, 1000)
	n, err := encoder.Encode(make([]int16, 320), silenceBuffer)
	if err != nil {
		return err
	}
	silence := silenceBuffer[:n]

	deviceID := fmt.Sprintf("pr281-e2e-%d", time.Now().Unix())
	header := http.Header{}
	header.Set("Device-Id", deviceID)
	header.Set("Client-Id", "pr281-e2e-client")
	header.Set("Protocol-Version", "1")
	header.Set("Authorization", "Bearer test-token")
	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		return fmt.Errorf("dial WebSocket: %w", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(timeout))
	decoder, err := opus.NewDecoder(16000, 1)
	if err != nil {
		return err
	}
	events := make(chan serverEvent, 1024)
	go func() {
		for {
			messageType, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				events <- serverEvent{Err: readErr}
				return
			}
			if messageType == websocket.BinaryMessage {
				pcm := make([]int16, 1920)
				samples, decodeErr := decoder.Decode(payload, pcm)
				event := serverEvent{Bytes: len(payload), Decoded: decodeErr == nil && samples > 0}
				for _, sample := range pcm[:max(samples, 0)] {
					if sample != 0 {
						event.Signal = true
						break
					}
				}
				events <- event
				continue
			}
			if messageType == websocket.TextMessage {
				var event serverEvent
				if json.Unmarshal(payload, &event) == nil {
					events <- event
				}
			}
		}
	}()
	if err := sendJSON(conn, map[string]any{
		"type": "hello", "device_id": deviceID, "transport": "websocket", "version": 1,
		"features": map[string]bool{},
		"audio_params": map[string]any{"sample_rate": 16000, "channels": 1, "frame_duration": 20, "format": "opus"},
	}); err != nil {
		return err
	}
	select {
	case event := <-events:
		if event.Err != nil || event.Type != "hello" {
			return fmt.Errorf("hello failed: type=%s err=%v", event.Type, event.Err)
		}
	case <-time.After(15 * time.Second):
		return errors.New("hello timeout")
	}
	if err := sendJSON(conn, map[string]any{"type": "listen", "device_id": deviceID, "state": "start", "mode": "auto"}); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	for _, frame := range frames {
		if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	for range 100 {
		if err := conn.WriteMessage(websocket.BinaryMessage, silence); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	fmt.Printf("audio sent: device=%s input_frames=%d silence_frames=100\n", deviceID, len(frames))
	var stt, sentence, audioFrames, audioBytes, decodedFrames, signalFrames int
	deadline := time.After(timeout)
	for {
		select {
		case event := <-events:
			if event.Err != nil {
				return fmt.Errorf("WebSocket closed before TTS stop: %w", event.Err)
			}
			if event.Bytes > 0 {
				audioFrames++
				audioBytes += event.Bytes
				if event.Decoded {
					decodedFrames++
				}
				if event.Signal {
					signalFrames++
				}
			}
			switch event.Type {
			case "stt":
				if event.Text != "" {
					stt++
				}
			case "tts":
				if event.State == "sentence_start" && event.Text != "" {
					sentence++
				}
				if event.State == "stop" {
					fmt.Printf("pipeline: stt=%d llm_sentence=%d tts_frames=%d decoded=%d signal=%d tts_bytes=%d tts_stop=true\n", stt, sentence, audioFrames, decodedFrames, signalFrames, audioBytes)
					if stt == 0 || sentence == 0 || audioFrames == 0 || audioBytes == 0 || decodedFrames != audioFrames || signalFrames == 0 {
						return errors.New("incomplete VAD/ASR/LLM/TTS chain")
					}
					return nil
				}
			}
		case <-deadline:
			fmt.Printf("pipeline timeout: stt=%d llm_sentence=%d tts_frames=%d tts_bytes=%d\n", stt, sentence, audioFrames, audioBytes)
			return errors.New("voice pipeline timeout")
		}
	}
}

func main() {
	url := flag.String("server", "ws://127.0.0.1:18989/xiaozhi/v1/", "WebSocket URL")
	wav := flag.String("wav", "", "16 kHz mono WAV fixture")
	timeout := flag.Duration("timeout", 120*time.Second, "overall response timeout")
	flag.Parse()
	if *wav == "" {
		fmt.Fprintln(os.Stderr, "-wav is required")
		os.Exit(2)
	}
	if err := run(*url, *wav, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "e2e failed:", err)
		os.Exit(1)
	}
	fmt.Println("e2e passed")
}
