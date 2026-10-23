// Command qwen_realtime_e2e sends a short WAV through the live Qwen audio pipeline.
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
	Type        string `json:"type"`
	State       string `json:"state"`
	Text        string `json:"text"`
	AudioParams *struct {
		SampleRate int `json:"sample_rate"`
		Channels   int `json:"channels"`
	} `json:"audio_params"`
	Bytes   int
	Decoded bool
	Signal  bool
	Err     error
	At      time.Time
}

func sendJSON(conn *websocket.Conn, data any) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, encoded)
}

func run(url, deviceID, wavPath string, timeout time.Duration, manual, duplex bool) error {
	if manual && duplex {
		return errors.New("manual and duplex modes cannot be combined")
	}
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

	header := http.Header{}
	header.Set("Device-Id", deviceID)
	header.Set("Client-Id", "qwen-realtime-e2e-client")
	header.Set("Protocol-Version", "1")
	header.Set("Authorization", "Bearer test-token")
	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		return fmt.Errorf("dial WebSocket: %w", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(timeout))
	decoder, err := opus.NewDecoder(24000, 1)
	if err != nil {
		return err
	}
	events := make(chan serverEvent, 4096)
	go func() {
		for {
			messageType, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				events <- serverEvent{Err: readErr, At: time.Now()}
				return
			}
			if messageType == websocket.BinaryMessage {
				pcm := make([]int16, 1920)
				samples, decodeErr := decoder.Decode(payload, pcm)
				event := serverEvent{Bytes: len(payload), Decoded: decodeErr == nil && samples > 0, At: time.Now()}
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
					event.At = time.Now()
					events <- event
				}
			}
		}
	}()
	if err := sendJSON(conn, map[string]any{
		"type": "hello", "device_id": deviceID, "transport": "websocket", "version": 1,
		"features":     map[string]bool{},
		"audio_params": map[string]any{"sample_rate": 16000, "channels": 1, "frame_duration": 20, "format": "opus"},
	}); err != nil {
		return err
	}
	select {
	case event := <-events:
		if event.Err != nil || event.Type != "hello" {
			return fmt.Errorf("hello failed: type=%s err=%v", event.Type, event.Err)
		}
		if event.AudioParams == nil || event.AudioParams.SampleRate != 24000 || event.AudioParams.Channels != 1 {
			return fmt.Errorf("unexpected output format: %+v", event.AudioParams)
		}
	case <-time.After(15 * time.Second):
		return errors.New("hello timeout")
	}
	listenMode := "auto"
	if duplex {
		listenMode = "realtime"
	}
	if err := sendJSON(conn, map[string]any{"type": "listen", "device_id": deviceID, "state": "start", "mode": listenMode}); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	for _, frame := range frames {
		if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	var stopSilence chan struct{}
	var silenceDone chan error
	silenceFrames := 0
	if duplex {
		stopSilence = make(chan struct{})
		silenceDone = make(chan error, 1)
		go func() {
			for {
				select {
				case <-stopSilence:
					silenceDone <- nil
					return
				default:
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, silence); err != nil {
					silenceDone <- err
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	} else {
		silenceFrames = 150
		if manual {
			silenceFrames = 20
		}
		for range silenceFrames {
			if err := conn.WriteMessage(websocket.BinaryMessage, silence); err != nil {
				return err
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if manual {
		if err := sendJSON(conn, map[string]any{"type": "listen", "device_id": deviceID, "state": "stop"}); err != nil {
			return err
		}
	}
	fmt.Printf("audio sent: device=%s input_frames=%d silence_frames=%d manual=%t duplex=%t\n", deviceID, len(frames), silenceFrames, manual, duplex)
	var stt, sentence, audioFrames, audioBytes, decodedFrames, signalFrames, ttsStops int
	var secondStartedAt, firstStopAt time.Time
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
				if duplex && secondStartedAt.IsZero() {
					close(stopSilence)
					if err := <-silenceDone; err != nil {
						return fmt.Errorf("silence stream: %w", err)
					}
					secondStartedAt = time.Now()
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
					fmt.Println("second audio sent during first reply")
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
					ttsStops++
					if firstStopAt.IsZero() {
						firstStopAt = event.At
					}
					if duplex && ttsStops < 2 {
						continue
					}
					fmt.Printf("pipeline: stt=%d reply_sentence=%d audio_frames=%d decoded=%d signal=%d audio_bytes=%d tts_stops=%d\n", stt, sentence, audioFrames, decodedFrames, signalFrames, audioBytes, ttsStops)
					if stt == 0 || sentence == 0 || audioFrames == 0 || audioBytes == 0 || decodedFrames != audioFrames || signalFrames == 0 {
						return errors.New("incomplete Qwen realtime audio chain")
					}
					if duplex && (stt < 2 || secondStartedAt.IsZero() || !secondStartedAt.Before(firstStopAt)) {
						return errors.New("second user turn did not overlap the first reply")
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
	url := flag.String("server", "ws://127.0.0.1:18991/xiaozhi/v1/", "WebSocket URL")
	deviceID := flag.String("device", "qwen-e2e-test-20261022", "activated test device ID")
	wav := flag.String("wav", "", "16 kHz mono WAV fixture")
	timeout := flag.Duration("timeout", 120*time.Second, "overall response timeout")
	manual := flag.Bool("manual", false, "send listen:stop to submit audio")
	duplex := flag.Bool("duplex", false, "send a second user turn while the first reply is playing")
	flag.Parse()
	if *wav == "" {
		fmt.Fprintln(os.Stderr, "-wav is required")
		os.Exit(2)
	}
	if err := run(*url, *deviceID, *wav, *timeout, *manual, *duplex); err != nil {
		fmt.Fprintln(os.Stderr, "e2e failed:", err)
		os.Exit(1)
	}
	fmt.Println("e2e passed")
}
