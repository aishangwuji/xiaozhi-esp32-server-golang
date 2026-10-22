package qwen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"testing"

	"xiaozhi-esp32-server-golang/internal/data/audio"
)

func TestNewQwenTTSProviderDefaultsAndSetVoice(t *testing.T) {
	provider := NewQwenTTSProvider(map[string]interface{}{})

	if provider.APIURL != defaultAPIURLBeijing {
		t.Fatalf("APIURL = %q", provider.APIURL)
	}
	if provider.Model != defaultQwenModel {
		t.Fatalf("Model = %q", provider.Model)
	}
	if provider.Voice != defaultQwenVoice {
		t.Fatalf("Voice = %q", provider.Voice)
	}
	if provider.LanguageType != defaultQwenLanguageType {
		t.Fatalf("LanguageType = %q", provider.LanguageType)
	}
	if provider.FrameDuration != audio.FrameDuration {
		t.Fatalf("FrameDuration = %d", provider.FrameDuration)
	}
	if !provider.IsValid() {
		t.Fatal("provider should be valid")
	}
	if err := provider.Close(); err != nil {
		t.Fatalf("Close error = %v", err)
	}

	if err := provider.SetVoice(map[string]interface{}{"voice": "Chelsie"}); err != nil {
		t.Fatalf("SetVoice error = %v", err)
	}
	if provider.Voice != "Chelsie" {
		t.Fatalf("Voice = %q", provider.Voice)
	}
	if err := provider.SetVoice(map[string]interface{}{}); err == nil {
		t.Fatal("expected missing voice to fail")
	}
}

func TestNewQwenTTSProviderSingaporeRegion(t *testing.T) {
	provider := NewQwenTTSProvider(map[string]interface{}{"region": "singapore"})
	if provider.APIURL != defaultAPIURLSingapore {
		t.Fatalf("APIURL = %q", provider.APIURL)
	}
}

func TestNewQwenTTSProviderDefaultsLanguageOnlyForQwenTTS(t *testing.T) {
	if got := NewQwenTTSProvider(map[string]interface{}{"model": "qwen3-tts-flash"}).LanguageType; got != "Chinese" {
		t.Fatalf("Qwen LanguageType = %q", got)
	}
	if got := NewQwenTTSProvider(map[string]interface{}{"model": "cosyvoice-v3-flash"}).LanguageType; got != "" {
		t.Fatalf("CosyVoice LanguageType = %q, want empty", got)
	}
	if got := NewQwenTTSProvider(map[string]interface{}{"model": "custom-tts"}).LanguageType; got != "" {
		t.Fatalf("custom LanguageType = %q, want empty", got)
	}
}

func TestNewQwenTTSProviderFormat(t *testing.T) {
	provider1 := NewQwenTTSProvider(map[string]interface{}{"stream": false})
	if provider1.Format != "wav" {
		t.Fatalf("expected format 'wav', got %q", provider1.Format)
	}

	provider2 := NewQwenTTSProvider(map[string]interface{}{"stream": true})
	if provider2.Format != "pcm" {
		t.Fatalf("expected format 'pcm', got %q", provider2.Format)
	}

	provider3 := NewQwenTTSProvider(map[string]interface{}{"format": "opus"})
	if provider3.Format != "opus" {
		t.Fatalf("expected format 'opus', got %q", provider3.Format)
	}
}

func TestCleanBase64RemovesWhitespace(t *testing.T) {
	got := cleanBase64(" YWJj\nZGU=\t")
	if got != "YWJjZGU=" {
		t.Fatalf("cleanBase64 = %q", got)
	}
}

func TestQwenDecoderFormatMatchesActualHTTPOutput(t *testing.T) {
	tests := []struct {
		model, format string
		stream        bool
		want          string
	}{
		{"qwen3-tts-flash", "ogg_opus", true, "pcm"},
		{"qwen3-tts-instruct-flash", "mp3", false, "wav"},
		{"qwen-audio-3.0-tts-flash", "ogg_opus", true, "ogg_opus"},
		{"cosyvoice-v3-flash", "mp3", false, "mp3"},
		{"cosyvoice-v3-flash", "", false, "wav"},
	}
	for _, tt := range tests {
		if got := qwenDecoderFormat(tt.model, tt.format, tt.stream); got != tt.want {
			t.Fatalf("qwenDecoderFormat(%q, %q, %v) = %q, want %q", tt.model, tt.format, tt.stream, got, tt.want)
		}
	}
}

func TestQwenAPIAudioFormatMapsOggOpus(t *testing.T) {
	if got := qwenAPIAudioFormat("ogg_opus"); got != "opus" {
		t.Fatalf("qwenAPIAudioFormat(ogg_opus) = %q", got)
	}
	if got := qwenAPIAudioFormat("opus"); got != "opus" {
		t.Fatalf("qwenAPIAudioFormat(opus) = %q", got)
	}
}

func TestPrepareQwenHTTPRequestUsesSpeechSynthesizerFieldsForQwenAudio(t *testing.T) {
	input := qwenRequestInput{
		Text:                 "hello",
		Voice:                "qwen-audio-3.0-tts-flash-demo",
		LanguageType:         "Chinese",
		Instructions:         "warm voice",
		OptimizeInstructions: true,
	}
	prepareQwenHTTPRequest("qwen-audio-3.0-tts-flash", "ogg_opus", false, &input)
	if input.Instruction != "warm voice" || input.Instructions != "" || input.OptimizeInstructions {
		t.Fatalf("instruction fields = %#v", input)
	}
	if input.LanguageType != "" || input.Format != "opus" || input.SampleRate != 24000 {
		t.Fatalf("SpeechSynthesizer input = %#v", input)
	}
}

func TestPrepareQwenHTTPRequestKeepsQwenTTSProtocol(t *testing.T) {
	input := qwenRequestInput{Text: "hello", Voice: "Cherry", LanguageType: "Chinese", Instructions: "warm voice", OptimizeInstructions: true}
	prepareQwenHTTPRequest("qwen3-tts-instruct-flash", "ogg_opus", true, &input)
	if input.Instructions != "warm voice" || input.Instruction != "" || input.Format != "" || input.SampleRate != 0 {
		t.Fatalf("Qwen-TTS input changed protocol: %#v", input)
	}
}

func TestApplyQwenVoicePromptUsesModelCapabilities(t *testing.T) {
	tests := []struct {
		name             string
		model            string
		voicePrompt      string
		wantModel        string
		wantInstructions string
	}{
		{name: "qwen flash upgrades for voice prompt", model: "qwen3-tts-flash", voicePrompt: "warm voice", wantModel: "qwen3-tts-instruct-flash", wantInstructions: "warm voice"},
		{name: "qwen flash stays unchanged without prompt", model: "qwen3-tts-flash", wantModel: "qwen3-tts-flash"},
		{name: "qwen instruct uses prompt", model: "qwen3-tts-instruct-flash", voicePrompt: "warm voice", wantModel: "qwen3-tts-instruct-flash", wantInstructions: "warm voice"},
		{name: "qwen audio uses prompt", model: "qwen-audio-3.0-tts-flash", voicePrompt: "warm voice", wantModel: "qwen-audio-3.0-tts-flash", wantInstructions: "warm voice"},
		{name: "cosyvoice ignores unsupported prompt", model: "cosyvoice-v3-flash", voicePrompt: "warm voice", wantModel: "cosyvoice-v3-flash"},
		{name: "unknown model stays conservative", model: "custom-tts", voicePrompt: "warm voice", wantModel: "custom-tts"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := qwenRequestInput{Text: "hello", Voice: "Cherry"}
			gotModel := applyQwenVoicePrompt(tt.model, &input, tt.voicePrompt)
			if gotModel != tt.wantModel || input.Instructions != tt.wantInstructions || input.Text != "hello" {
				t.Fatalf("model=%q input=%#v", gotModel, input)
			}
		})
	}
}

func TestNormalizeLeadingQwenAudioStripsWAVHeader(t *testing.T) {
	payload := []byte{1, 2, 3, 4}
	wav := makeTestWAV(payload)

	normalized, needMore, detectedWAV, err := normalizeLeadingQwenAudio(wav)
	if err != nil {
		t.Fatalf("normalizeLeadingQwenAudio error = %v", err)
	}
	if needMore {
		t.Fatal("needMore = true")
	}
	if !detectedWAV {
		t.Fatal("detectedWAV = false")
	}
	if !bytes.Equal(normalized, payload) {
		t.Fatalf("normalized = %v", normalized)
	}
}

func TestNormalizeLeadingQwenAudioKeepsPCM(t *testing.T) {
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

	normalized, needMore, detectedWAV, err := normalizeLeadingQwenAudio(pcm)
	if err != nil {
		t.Fatalf("normalizeLeadingQwenAudio error = %v", err)
	}
	if needMore || detectedWAV {
		t.Fatalf("needMore=%v detectedWAV=%v", needMore, detectedWAV)
	}
	if !bytes.Equal(normalized, pcm) {
		t.Fatalf("normalized = %v", normalized)
	}
}

func TestParseEventStreamPreservesWAVOnlyForWAVDecoder(t *testing.T) {
	pcm := []byte{1, 2, 3, 4}
	wav := makeTestWAV(pcm)
	stream := fmt.Sprintf("data: {\"output\":{\"audio\":{\"data\":%q},\"finish_reason\":\"stop\"}}\n\n", base64.StdEncoding.EncodeToString(wav))
	for _, tt := range []struct {
		format string
		want   []byte
	}{{"wav", wav}, {"pcm", pcm}} {
		t.Run(tt.format, func(t *testing.T) {
			reader, writer := io.Pipe()
			result := make(chan []byte, 1)
			go func() {
				data, _ := io.ReadAll(reader)
				result <- data
			}()
			err := (&QwenTTSProvider{}).parseEventStream(context.Background(), strings.NewReader(stream), writer, "test", tt.format)
			writer.Close()
			if err != nil {
				t.Fatal(err)
			}
			if got := <-result; !bytes.Equal(got, tt.want) {
				t.Fatalf("decoded stream = %v, want %v", got, tt.want)
			}
		})
	}
}

func makeTestWAV(payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+len(payload)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(24000))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(48000))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(payload)))
	buf.Write(payload)
	return buf.Bytes()
}
