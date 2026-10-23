package chat

import (
	"testing"

	"xiaozhi-esp32-server-golang/internal/domain/realtime"
)

func TestRealtimeSmartTurnKeepsPlaybackOnProvisionalSpeech(t *testing.T) {
	r := &realtimeAudioSession{cfg: realtime.Config{TurnDetection: "smart_turn"}, outputPCM: []int16{1, 2}}
	r.responseActive.Store(true)

	r.handleEvent(realtime.Event{Type: "input_audio_buffer.speech_started"})
	r.handleEvent(realtime.Event{Type: "input_audio_buffer.speech_stopped", Reason: "turn_invalid"})

	if r.skipUntilDone.Load() || len(r.outputPCM) != 2 {
		t.Fatal("smart_turn invalid speech interrupted the active response")
	}
}

func TestRealtimeServerVADInterruptsOnSpeechStart(t *testing.T) {
	r := &realtimeAudioSession{cfg: realtime.Config{TurnDetection: "server_vad"}, outputPCM: []int16{1, 2}}
	r.responseActive.Store(true)

	r.handleEvent(realtime.Event{Type: "input_audio_buffer.speech_started"})

	if !r.skipUntilDone.Load() || len(r.outputPCM) != 0 {
		t.Fatal("server_vad speech did not interrupt the active response")
	}
}

func TestRealtimeCancelledResponseDiscardsBufferedAudio(t *testing.T) {
	r := &realtimeAudioSession{cfg: realtime.Config{TurnDetection: "smart_turn"}, outputPCM: []int16{1, 2}}
	r.responseActive.Store(true)

	r.handleEvent(realtime.Event{Type: "response.done", Status: "cancelled"})

	if r.responseActive.Load() || r.skipUntilDone.Load() || len(r.outputPCM) != 0 {
		t.Fatal("cancelled smart_turn response left buffered audio or response state")
	}
}
