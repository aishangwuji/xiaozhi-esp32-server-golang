package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestParseAliyunQwenConfig(t *testing.T) {
	cfg, err := ParseAliyunQwenConfig(map[string]interface{}{
		"api_key":        "test-key",
		"turn_detection": "null",
		"model":          "test-model",
	})
	if err != nil {
		t.Fatalf("ParseAliyunQwenConfig() error = %v", err)
	}
	if cfg.APIKey != "test-key" || cfg.Model != "test-model" || cfg.TurnDetection != "null" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if _, err := ParseAliyunQwenConfig(map[string]interface{}{}); err == nil {
		t.Fatal("missing api_key should fail")
	}
	if _, err := ParseAliyunQwenConfig(map[string]interface{}{
		"api_key": "test-key", "input_sample_rate": 24000,
	}); err == nil {
		t.Fatal("unsupported Qwen input sample rate should fail")
	}
}

func TestAliyunQwenClientProtocol(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	firstMessage := make(chan map[string]interface{}, 1)
	commands := make(chan map[string]interface{}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("model") != "test-model" {
			http.Error(w, "missing model", http.StatusBadRequest)
			return
		}
		if req.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]interface{}{"type": "session.created"})
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var session map[string]interface{}
		if json.Unmarshal(data, &session) == nil {
			firstMessage <- session
		}
		_ = conn.WriteJSON(map[string]interface{}{"type": "session.updated"})
		audio := base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4})
		_ = conn.WriteJSON(map[string]interface{}{"type": "response.created", "response": map[string]string{"id": "resp-1"}})
		_ = conn.WriteJSON(map[string]interface{}{"type": "response.audio.delta", "delta": audio})
		_ = conn.WriteJSON(map[string]interface{}{"type": "response.done", "response": map[string]string{"id": "resp-1"}})
		for i := 0; i < 3; i++ {
			_, data, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			var command map[string]interface{}
			if json.Unmarshal(data, &command) == nil {
				commands <- command
			}
		}
	}))
	defer server.Close()

	cfg := Config{
		APIKey:        "test-key",
		BaseURL:       strings.Replace(server.URL, "http://", "ws://", 1),
		Model:         "test-model",
		Voice:         DefaultAliyunQwenRealtimeVoice,
		TurnDetection: "null",
	}
	client := NewAliyunQwenClient(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer client.Close()

	select {
	case session := <-firstMessage:
		if session["type"] != "session.update" {
			t.Fatalf("first message type = %v", session["type"])
		}
		configuration, ok := session["session"].(map[string]interface{})
		if !ok {
			t.Fatalf("session payload = %#v", session["session"])
		}
		if configuration["input_audio_format"] != "pcm" || configuration["output_audio_format"] != "pcm" {
			t.Fatalf("unexpected Qwen audio formats: %#v", configuration)
		}
	case <-ctx.Done():
		t.Fatal("server did not receive session.update")
	}

	var audioEvent Event
	for audioEvent.Type != "response.audio.delta" {
		select {
		case audioEvent = <-client.Events():
		case <-ctx.Done():
			t.Fatal("did not receive audio event")
		}
	}
	if audioEvent.Type != "response.audio.delta" || string(audioEvent.Audio) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("unexpected audio event: %+v", audioEvent)
	}

	if err := client.SendAudio([]byte{9, 8}); err != nil {
		t.Fatalf("SendAudio() error = %v", err)
	}
	if err := client.CommitAudio(); err != nil {
		t.Fatalf("CommitAudio() error = %v", err)
	}
	for i := 0; i < 3; i++ {
		select {
		case command := <-commands:
			if i == 0 && command["type"] != "input_audio_buffer.append" {
				t.Fatalf("first command type = %v", command["type"])
			}
			if i == 1 && command["type"] != "input_audio_buffer.commit" {
				t.Fatalf("second command type = %v", command["type"])
			}
			if i == 2 && command["type"] != "response.create" {
				t.Fatalf("third command type = %v", command["type"])
			}
		case <-ctx.Done():
			t.Fatal("server did not receive client audio commands")
		}
	}
}

func TestAliyunQwenClientWaitsForSessionUpdatedAndInjectsHistory(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	allowReady := make(chan struct{})
	history := make(chan map[string]interface{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]interface{}{"type": "session.created"})
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		<-allowReady
		_ = conn.WriteJSON(map[string]interface{}{"type": "session.updated"})
		for i := 0; i < 2; i++ {
			_, data, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			var event map[string]interface{}
			if json.Unmarshal(data, &event) == nil {
				history <- event
			}
		}
	}))
	defer server.Close()

	client := NewAliyunQwenClient(Config{
		APIKey:        "test-key",
		BaseURL:       strings.Replace(server.URL, "http://", "ws://", 1),
		Model:         "test-model",
		Voice:         DefaultAliyunQwenRealtimeVoice,
		TurnDetection: "smart_turn",
		InitialMessages: []ConversationMessage{
			{Role: "user", Text: "上一轮问题"},
			{Role: "assistant", Text: "上一轮回答"},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- client.Start(ctx) }()
	select {
	case err := <-started:
		t.Fatalf("Start returned before session.updated: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowReady)
	if err := <-started; err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer client.Close()

	for i, wantRole := range []string{"user", "assistant"} {
		select {
		case event := <-history:
			if event["type"] != "conversation.item.create" {
				t.Fatalf("history event %d type = %v", i, event["type"])
			}
			item := event["item"].(map[string]interface{})
			if item["role"] != wantRole {
				t.Fatalf("history event %d role = %v, want %s", i, item["role"], wantRole)
			}
		case <-ctx.Done():
			t.Fatal("server did not receive history messages")
		}
	}
}

func TestAliyunQwenClientDefersCancelUntilResponseCreated(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	createResponse := make(chan struct{})
	command := make(chan map[string]interface{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]interface{}{"type": "session.created"})
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]interface{}{"type": "session.updated"})
		<-createResponse
		_ = conn.WriteJSON(map[string]interface{}{
			"type":     "response.created",
			"response": map[string]interface{}{"id": "response-cancel"},
		})
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var event map[string]interface{}
		if json.Unmarshal(data, &event) == nil {
			command <- event
		}
	}))
	defer server.Close()

	client := NewAliyunQwenClient(Config{
		APIKey: "test-key", BaseURL: strings.Replace(server.URL, "http://", "ws://", 1), Model: "test-model",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer client.Close()
	if err := client.CancelResponse(); err != nil {
		t.Fatalf("CancelResponse() error = %v", err)
	}
	close(createResponse)
	select {
	case event := <-command:
		if event["type"] != "response.cancel" {
			t.Fatalf("deferred command type = %v", event["type"])
		}
	case <-ctx.Done():
		t.Fatal("server did not receive deferred response.cancel")
	}
}

func TestAliyunQwenClientTranscriptDeltaUsesDeltaField(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]interface{}{"type": "session.created"})
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]interface{}{"type": "session.updated"})
		_ = conn.WriteJSON(map[string]interface{}{
			"type":        "response.audio_transcript.delta",
			"response_id": "resp-text-1",
			"item_id":     "item-text-1",
			"delta":       "你好，这是增量文本",
		})
		_ = conn.WriteJSON(map[string]interface{}{
			"type":        "response.audio_transcript.done",
			"response_id": "resp-text-1",
			"transcript":  "你好，这是增量文本",
		})
	}))
	defer server.Close()

	client := NewAliyunQwenClient(Config{
		APIKey:        "test-key",
		BaseURL:       strings.Replace(server.URL, "http://", "ws://", 1),
		Model:         "test-model",
		TurnDetection: "smart_turn",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer client.Close()

	for {
		select {
		case event := <-client.Events():
			if event.Type != "response.audio_transcript.delta" {
				continue
			}
			if event.Type != "response.audio_transcript.delta" || event.Text != "你好，这是增量文本" {
				t.Fatalf("unexpected transcript event: %+v", event)
			}
			if event.ResponseID != "resp-text-1" || event.ItemID != "item-text-1" {
				t.Fatalf("missing transcript identifiers: %+v", event)
			}
			return
		case <-ctx.Done():
			t.Fatal("did not receive transcript delta")
		}
	}
}
