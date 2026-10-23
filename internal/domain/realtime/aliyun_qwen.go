package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	ProviderAliyunQwenRealtime     = "aliyun_qwen_realtime"
	DefaultAliyunQwenRealtimeURL   = "wss://dashscope.aliyuncs.com/api-ws/v1/realtime"
	DefaultAliyunQwenRealtimeModel = "qwen-audio-3.1-realtime-plus"
	DefaultAliyunQwenRealtimeVoice = "longanqian_v3.1"
)

// Config is the provider-neutral subset used by the audio bridge. The map
// based config is deliberately kept at the manager boundary so future live
// providers can use the same agent/config model without changing UConfig.
type Config struct {
	Provider          string
	APIKey            string
	BaseURL           string
	Workspace         string
	Model             string
	Voice             string
	Instructions      string
	TurnDetection     string
	InputSampleRate   int
	OutputSampleRate  int
	Channels          int
	FrameDurationMs   int
	SilenceDurationMs int
	InitialMessages   []ConversationMessage
}

// ConversationMessage is the text-only history shared by realtime providers.
// Provider adapters translate it to their native startup/context events.
type ConversationMessage struct {
	Role string
	Text string
}

func ParseAliyunQwenConfig(raw map[string]interface{}) (Config, error) {
	cfg := Config{
		BaseURL:          DefaultAliyunQwenRealtimeURL,
		Model:            DefaultAliyunQwenRealtimeModel,
		Voice:            DefaultAliyunQwenRealtimeVoice,
		TurnDetection:    "smart_turn",
		InputSampleRate:  16000,
		OutputSampleRate: 24000,
		Channels:         1,
		FrameDurationMs:  20,
	}
	if raw == nil {
		return cfg, errors.New("realtime 配置为空")
	}
	cfg.APIKey = stringValue(raw, "api_key")
	if cfg.APIKey == "" {
		cfg.APIKey = stringValue(raw, "key")
	}
	if value := stringValue(raw, "base_url"); value != "" {
		cfg.BaseURL = value
	}
	cfg.Workspace = stringValue(raw, "workspace")
	if value := stringValue(raw, "model"); value != "" {
		cfg.Model = value
	}
	if value := stringValue(raw, "voice"); value != "" {
		cfg.Voice = value
	}
	if value := stringValue(raw, "instructions"); value != "" {
		cfg.Instructions = value
	}
	if value := stringValue(raw, "turn_detection"); value != "" {
		cfg.TurnDetection = strings.ToLower(strings.TrimSpace(value))
	}
	if cfg.TurnDetection != "smart_turn" && cfg.TurnDetection != "server_vad" && cfg.TurnDetection != "null" {
		return Config{}, fmt.Errorf("不支持的 turn_detection: %s", cfg.TurnDetection)
	}
	cfg.InputSampleRate = intValue(raw, "input_sample_rate", cfg.InputSampleRate)
	cfg.OutputSampleRate = intValue(raw, "output_sample_rate", cfg.OutputSampleRate)
	cfg.Channels = intValue(raw, "channels", cfg.Channels)
	cfg.FrameDurationMs = intValue(raw, "frame_duration_ms", cfg.FrameDurationMs)
	cfg.SilenceDurationMs = intValue(raw, "silence_duration_ms", 0)
	if cfg.APIKey == "" {
		return Config{}, errors.New("缺少 api_key")
	}
	if cfg.InputSampleRate != 16000 || cfg.OutputSampleRate != 24000 || cfg.Channels != 1 {
		return Config{}, fmt.Errorf("Qwen Realtime 仅支持 16kHz PCM16 单声道输入和 24kHz PCM16 单声道输出")
	}
	if cfg.TurnDetection == "server_vad" && cfg.SilenceDurationMs > 0 && (cfg.SilenceDurationMs < 200 || cfg.SilenceDurationMs > 6000) {
		return Config{}, fmt.Errorf("Qwen Realtime silence_duration_ms 必须在 200-6000 之间")
	}
	cfg.Provider = ProviderAliyunQwenRealtime
	return cfg, nil
}

func stringValue(raw map[string]interface{}, key string) string {
	value, _ := raw[key].(string)
	return strings.TrimSpace(value)
}

func intValue(raw map[string]interface{}, key string, fallback int) int {
	switch value := raw[key].(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 {
			return int(value)
		}
	case json.Number:
		if parsed, err := value.Int64(); err == nil && parsed > 0 {
			return int(parsed)
		}
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &parsed); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

type Event struct {
	Type       string
	Audio      []byte
	Text       string
	ResponseID string
	ItemID     string
	Status     string
	Reason     string
	StartMs    int64
	EndMs      int64
	Err        error
	Fatal      bool
}

// Client is a small, provider-specific transport client. It does not know
// anything about local VAD/ASR/LLM/TTS and only exposes realtime events.
type AliyunQwenClient struct {
	cfg            Config
	conn           *websocket.Conn
	writeMu        sync.Mutex
	events         chan Event
	done           chan struct{}
	created        chan error
	ready          chan error
	createdOnce    sync.Once
	readyOnce      sync.Once
	closeOnce      sync.Once
	responseActive atomic.Bool
	cancelPending  atomic.Bool
}

func NewAliyunQwenClient(cfg Config) *AliyunQwenClient {
	return &AliyunQwenClient{
		cfg:     cfg,
		events:  make(chan Event, 64),
		done:    make(chan struct{}),
		created: make(chan error, 1),
		ready:   make(chan error, 1),
	}
}

func (c *AliyunQwenClient) Events() <-chan Event { return c.events }

func (c *AliyunQwenClient) Start(ctx context.Context) error {
	if c == nil {
		return errors.New("realtime client 为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint, err := url.Parse(c.cfg.BaseURL)
	if err != nil {
		return fmt.Errorf("解析 realtime endpoint 失败: %w", err)
	}
	query := endpoint.Query()
	query.Set("model", c.cfg.Model)
	endpoint.RawQuery = query.Encode()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	if c.cfg.Workspace != "" {
		header.Set("X-DashScope-WorkSpace", c.cfg.Workspace)
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint.String(), header)
	if err != nil {
		return fmt.Errorf("连接阿里云 Qwen Realtime 失败: %w", err)
	}
	c.conn = conn
	go c.readLoop()
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	select {
	case err := <-c.created:
		if err != nil {
			_ = c.Close()
			return err
		}
	case <-waitCtx.Done():
		_ = c.Close()
		return fmt.Errorf("等待 Qwen Realtime session.created 超时: %w", waitCtx.Err())
	}
	if err := c.send(map[string]interface{}{
		"type": "session.update",
		"session": map[string]interface{}{
			"modalities":          []string{"text", "audio"},
			"voice":               c.cfg.Voice,
			"instructions":        c.cfg.Instructions,
			"input_audio_format":  "pcm",
			"output_audio_format": "pcm",
			"turn_detection":      c.turnDetectionPayload(),
		},
	}); err != nil {
		_ = conn.Close()
		return err
	}
	select {
	case err := <-c.ready:
		if err != nil {
			_ = c.Close()
			return err
		}
		if err := c.sendInitialMessages(); err != nil {
			_ = c.Close()
			return err
		}
		return nil
	case <-waitCtx.Done():
		_ = c.Close()
		return fmt.Errorf("等待 Qwen Realtime session.updated 超时: %w", waitCtx.Err())
	}
}

func (c *AliyunQwenClient) turnDetectionPayload() interface{} {
	if c.cfg.TurnDetection == "null" {
		return nil
	}
	payload := map[string]interface{}{"type": c.cfg.TurnDetection}
	if c.cfg.TurnDetection == "server_vad" && c.cfg.SilenceDurationMs > 0 {
		payload["silence_duration_ms"] = c.cfg.SilenceDurationMs
	}
	return payload
}

func (c *AliyunQwenClient) sendInitialMessages() error {
	for _, message := range c.cfg.InitialMessages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		text := strings.TrimSpace(message.Text)
		if text == "" || (role != "system" && role != "user" && role != "assistant") {
			continue
		}
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		if err := c.send(map[string]interface{}{
			"type": "conversation.item.create",
			"item": map[string]interface{}{
				"type": "message",
				"role": role,
				"content": []map[string]interface{}{{
					"type": contentType,
					"text": text,
				}},
			},
		}); err != nil {
			return fmt.Errorf("注入 Qwen Realtime 历史消息失败: %w", err)
		}
	}
	return nil
}

func (c *AliyunQwenClient) send(payload interface{}) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn == nil {
		return errors.New("realtime websocket 未连接")
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(websocketWriteWait)); err != nil {
		return err
	}
	return c.conn.WriteJSON(payload)
}

func (c *AliyunQwenClient) SendAudio(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	return c.send(map[string]interface{}{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(pcm),
	})
}

// StartAudioActivity is intentionally a no-op for Qwen Realtime. Its native
// protocol starts accepting audio as soon as input_audio_buffer.append is
// received; manual turn detection only needs the commit sent below.
func (c *AliyunQwenClient) StartAudioActivity() error { return nil }

func (c *AliyunQwenClient) CommitAudio() error {
	if err := c.send(map[string]interface{}{"type": "input_audio_buffer.commit"}); err != nil {
		return err
	}
	return c.CreateResponse()
}

func (c *AliyunQwenClient) ClearAudio() error {
	return c.send(map[string]interface{}{"type": "input_audio_buffer.clear"})
}

func (c *AliyunQwenClient) CreateResponse() error {
	return c.send(map[string]interface{}{"type": "response.create"})
}

func (c *AliyunQwenClient) CancelResponse() error {
	if !c.responseActive.Load() {
		// response.cancel before response.created is rejected by Qwen. Remember
		// the local abort and cancel as soon as the pending response becomes
		// addressable instead of allowing it to play after the caller aborted.
		c.cancelPending.Store(true)
		return nil
	}
	c.cancelPending.Store(false)
	return c.send(map[string]interface{}{"type": "response.cancel"})
}

func (c *AliyunQwenClient) Close() error {
	if c == nil {
		return nil
	}
	var err error
	c.closeOnce.Do(func() {
		if c.done != nil {
			close(c.done)
		}
		c.writeMu.Lock()
		if c.conn != nil {
			err = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(websocketWriteWait))
			errClose := c.conn.Close()
			if err == nil {
				err = errClose
			}
		}
		c.writeMu.Unlock()
	})
	return err
}

const websocketWriteWait = 2 * time.Second

func (c *AliyunQwenClient) readLoop() {
	defer close(c.events)
	defer c.signalCreated(errors.New("Qwen Realtime 连接已关闭"))
	defer c.signalReady(errors.New("Qwen Realtime 连接已关闭"))
	for {
		if c.conn == nil {
			return
		}
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if !isClosed(c.done) {
				c.emitEvent(Event{Type: "error", Err: err, Fatal: true})
			}
			return
		}
		var raw struct {
			Type       string `json:"type"`
			Delta      string `json:"delta"`
			Text       string `json:"text"`
			Transcript string `json:"transcript"`
			ResponseID string `json:"response_id"`
			Response   struct {
				ID            string `json:"id"`
				Status        string `json:"status"`
				StatusDetails struct {
					Reason string `json:"reason"`
				} `json:"status_details"`
			} `json:"response"`
			ItemID       string `json:"item_id"`
			Reason       string `json:"reason"`
			AudioStartMs int64  `json:"audio_start_ms"`
			AudioEndMs   int64  `json:"audio_end_ms"`
			Error        struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
				Param   string `json:"param"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			parseErr := fmt.Errorf("解析 Qwen Realtime 事件失败: %w", err)
			c.signalCreated(parseErr)
			c.signalReady(parseErr)
			if !c.emitEvent(Event{Type: "error", Err: parseErr}) {
				return
			}
			continue
		}
		text := raw.Transcript
		if text == "" {
			text = raw.Delta
		}
		if text == "" {
			text = raw.Text
		}
		responseID := raw.ResponseID
		if responseID == "" {
			responseID = raw.Response.ID
		}
		reason := raw.Reason
		if reason == "" {
			reason = raw.Response.StatusDetails.Reason
		}
		event := Event{
			Type:       raw.Type,
			Text:       text,
			ResponseID: responseID,
			ItemID:     raw.ItemID,
			Status:     raw.Response.Status,
			Reason:     reason,
			StartMs:    raw.AudioStartMs,
			EndMs:      raw.AudioEndMs,
		}
		switch raw.Type {
		case "session.created":
			c.signalCreated(nil)
		case "session.updated":
			c.signalReady(nil)
			continue
		case "response.created":
			c.responseActive.Store(true)
			if c.cancelPending.Swap(false) {
				if err := c.send(map[string]interface{}{"type": "response.cancel"}); err != nil {
					event.Err = fmt.Errorf("取消待处理的 Qwen Realtime 响应失败: %w", err)
				}
			}
		case "response.done":
			c.responseActive.Store(false)
			c.cancelPending.Store(false)
			if strings.EqualFold(event.Status, "failed") {
				event.Err = fmt.Errorf("Qwen Realtime 响应失败: reason=%s", event.Reason)
			}
		case "response.text.delta":
			event.Type = "response.audio_transcript.delta"
		case "response.text.done":
			event.Type = "response.audio_transcript.done"
		case "conversation.item.input_audio_transcription.failed":
			message := strings.TrimSpace(raw.Error.Message)
			if message == "" {
				message = "未知错误"
			}
			event.Err = fmt.Errorf("Qwen Realtime 输入音频转写失败: code=%s message=%s", raw.Error.Code, message)
		}
		if raw.Type == "response.audio.delta" && raw.Delta != "" {
			decoded, decodeErr := base64.StdEncoding.DecodeString(raw.Delta)
			if decodeErr != nil {
				event.Err = fmt.Errorf("解析 Qwen 音频增量失败: %w", decodeErr)
			} else {
				event.Audio = decoded
			}
		}
		if raw.Type == "error" {
			message := strings.TrimSpace(raw.Error.Message)
			if message == "" {
				message = "未知错误"
			}
			event.Err = fmt.Errorf("Qwen Realtime 返回错误: type=%s code=%s param=%s message=%s",
				raw.Error.Type, raw.Error.Code, raw.Error.Param, message)
			event.Fatal = raw.Error.Type == "server_error"
			c.signalCreated(event.Err)
			c.signalReady(event.Err)
		}
		if !c.emitEvent(event) {
			return
		}
	}
}

func (c *AliyunQwenClient) signalCreated(err error) {
	if c == nil || c.created == nil {
		return
	}
	c.createdOnce.Do(func() { c.created <- err })
}

func (c *AliyunQwenClient) signalReady(err error) {
	if c == nil || c.ready == nil {
		return
	}
	c.readyOnce.Do(func() { c.ready <- err })
}

func (c *AliyunQwenClient) emitEvent(event Event) bool {
	if c == nil {
		return false
	}
	if c.done == nil {
		c.events <- event
		return true
	}
	select {
	case c.events <- event:
		return true
	case <-c.done:
		return false
	}
}
