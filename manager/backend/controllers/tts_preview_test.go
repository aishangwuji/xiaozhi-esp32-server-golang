package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"xiaozhi/manager/backend/models"
)

type previewRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn previewRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func previewTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Config{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestResolveUserPreviewUsesOnlyEnabledAliyunConfig(t *testing.T) {
	db := previewTestDB(t)
	config := models.Config{
		Type: "tts", ConfigID: "aliyun_enabled", Provider: "aliyun_qwen", Enabled: true,
		JsonData: `{"api_key":"stored-key","api_url":"https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation","model":"qwen3-tts-flash","voice":"Ethan"}`,
	}
	if err := db.Create(&config).Error; err != nil {
		t.Fatal(err)
	}
	controller := NewTTSPreviewController(db)

	resolved, err := controller.resolvePreviewRequest(TTSPreviewRequest{
		Provider: "aliyun_qwen", ConfigID: config.ConfigID, APIKey: "attacker-key", APIURL: "https://example.com/private", Model: "attacker-model", Voice: "Serena",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.APIKey != "stored-key" || resolved.APIURL != "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation" || resolved.Model != "qwen3-tts-flash" {
		t.Fatalf("user overrides leaked into resolved config: %#v", resolved)
	}
	if resolved.Voice != "Serena" {
		t.Fatalf("role voice override = %q", resolved.Voice)
	}
	storedVoice, err := controller.resolvePreviewRequest(TTSPreviewRequest{Provider: "aliyun_qwen", ConfigID: config.ConfigID}, true)
	if err != nil {
		t.Fatal(err)
	}
	if storedVoice.Voice != "Ethan" {
		t.Fatalf("stored voice = %q", storedVoice.Voice)
	}
}

func TestResolveUserPreviewRejectsDisabledAndNonAliyunConfigs(t *testing.T) {
	db := previewTestDB(t)
	configs := []models.Config{
		{Type: "tts", ConfigID: "disabled", Provider: "aliyun_qwen", Enabled: false, JsonData: `{"api_key":"key"}`},
		{Type: "tts", ConfigID: "minimax", Provider: "minimax", Enabled: true, JsonData: `{"api_key":"key"}`},
	}
	if err := db.Create(&configs).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.Config{}).Where("config_id = ?", "disabled").Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	controller := NewTTSPreviewController(db)
	for _, configID := range []string{"disabled", "minimax"} {
		if _, err := controller.resolvePreviewRequest(TTSPreviewRequest{Provider: "aliyun_qwen", ConfigID: configID}, true); err == nil {
			t.Fatalf("config %q unexpectedly allowed", configID)
		}
	}
}

func TestResolveAdminPreviewUsesRegionEndpointAndRejectsOtherProviders(t *testing.T) {
	controller := NewTTSPreviewController(nil)
	resolved, err := controller.resolvePreviewRequest(TTSPreviewRequest{Provider: "aliyun_qwen", Region: "singapore", APIKey: "key", Model: "qwen3-tts-flash"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.APIURL != defaultAliyunPreviewAPIURLSingapore {
		t.Fatalf("APIURL = %q", resolved.APIURL)
	}
	if resolved.LanguageType != "Chinese" {
		t.Fatalf("LanguageType = %q", resolved.LanguageType)
	}
	if _, err := controller.resolvePreviewRequest(TTSPreviewRequest{Provider: "cosyvoice", APIKey: "key"}, false); err == nil {
		t.Fatal("local cosyvoice preview unexpectedly allowed")
	}
}

func TestAliyunPreviewURLValidation(t *testing.T) {
	for _, raw := range []string{
		"http://dashscope.aliyuncs.com/api",
		"https://127.0.0.1/api",
		"https://localhost/api",
		"https://example.com/api",
	} {
		if err := validateAliyunPreviewURL(raw, false); err == nil {
			t.Fatalf("API URL %q unexpectedly allowed", raw)
		}
	}
	if err := validateAliyunPreviewURL("https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation", false); err != nil {
		t.Fatal(err)
	}
	if err := validateAliyunPreviewURL("https://dashscope-result.oss-cn-shanghai.aliyuncs.com/audio.wav", true); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"https://evil.aliyuncs.com/audio.wav",
		"https://dashscope-result.oss-cn-shanghai.aliyuncs.com.evil.test/audio.wav",
		"https://127.0.0.1/audio.wav",
		"https://dashscope-result.oss-cn-shanghai.aliyuncs.com:8443/audio.wav",
		"https://user@dashscope-result.oss-cn-shanghai.aliyuncs.com/audio.wav",
	} {
		if _, err := normalizeAliyunPreviewAudioURL(raw); err == nil {
			t.Fatalf("audio URL %q unexpectedly allowed", raw)
		}
	}
	got, err := normalizeAliyunPreviewAudioURL("http://dashscope-result-bj.oss-cn-beijing.aliyuncs.com/a.wav?Expires=123&Signature=abc")
	if err != nil || got != "https://dashscope-result-bj.oss-cn-beijing.aliyuncs.com/a.wav?Expires=123&Signature=abc" {
		t.Fatalf("normalized audio URL = %q, err = %v", got, err)
	}
}

func TestAliyunPreviewRedirectsStayOnHTTPSOSS(t *testing.T) {
	controller := NewTTSPreviewController(nil)
	apiRequest, _ := http.NewRequest(http.MethodPost, defaultAliyunPreviewAPIURLBeijing, nil)
	ossURL, _ := url.Parse("http://dashscope-result-bj.oss-cn-beijing.aliyuncs.com/audio.wav?Signature=abc")
	redirect := &http.Request{Method: http.MethodGet, URL: ossURL}
	if err := controller.HTTPClient.CheckRedirect(redirect, []*http.Request{apiRequest}); err == nil {
		t.Fatal("API redirect to OSS unexpectedly allowed")
	}
	audioRequest, _ := http.NewRequest(http.MethodGet, "https://dashscope-result-bj.oss-cn-beijing.aliyuncs.com/start.wav", nil)
	if err := controller.HTTPClient.CheckRedirect(redirect, []*http.Request{audioRequest}); err != nil {
		t.Fatal(err)
	}
	if redirect.URL.Scheme != "https" || redirect.URL.RawQuery != "Signature=abc" {
		t.Fatalf("redirect URL = %s", redirect.URL)
	}
	redirect.URL, _ = url.Parse("https://evil.example.com/audio.wav")
	if err := controller.HTTPClient.CheckRedirect(redirect, []*http.Request{audioRequest}); err == nil {
		t.Fatal("cross-domain audio redirect unexpectedly allowed")
	}
}

func TestBuildAliyunPreviewRequestBodyUsesModelCapabilities(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		instruction  string
		wantModel    string
		wantPlural   bool
		wantSingular bool
	}{
		{name: "qwen flash upgrades", model: "qwen3-tts-flash", instruction: "warm voice", wantModel: "qwen3-tts-instruct-flash", wantPlural: true},
		{name: "qwen audio accepts instruction", model: "qwen-audio-3.0-tts-flash", instruction: "warm voice", wantModel: "qwen-audio-3.0-tts-flash", wantSingular: true},
		{name: "legacy qwen instruct accepts instruction", model: "qwen-tts-instruct", instruction: "warm voice", wantModel: "qwen-tts-instruct", wantPlural: true},
		{name: "versioned qwen flash is not silently changed", model: "qwen3-tts-flash-2025-11-27", instruction: "warm voice", wantModel: "qwen3-tts-flash-2025-11-27"},
		{name: "aliyun cosyvoice rejects qwen instruction", model: "cosyvoice-v3-flash", instruction: "warm voice", wantModel: "cosyvoice-v3-flash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := buildAliyunPreviewRequestBody(TTSPreviewRequest{Model: tt.model, Voice: "Cherry", Text: "hello", Instruction: tt.instruction, LanguageType: "English"})
			if body["model"] != tt.wantModel {
				t.Fatalf("model = %v", body["model"])
			}
			input := body["input"].(map[string]any)
			_, hasPlural := input["instructions"]
			if hasPlural != tt.wantPlural {
				t.Fatalf("input = %#v", input)
			}
			_, hasSingular := input["instruction"]
			if hasSingular != tt.wantSingular {
				t.Fatalf("input = %#v", input)
			}
			if strings.HasPrefix(tt.model, "qwen-audio-") || strings.HasPrefix(tt.model, "cosyvoice") {
				if input["format"] != "wav" || input["sample_rate"] != 24000 {
					t.Fatalf("SpeechSynthesizer fields = %#v", input)
				}
				if _, ok := input["language_type"]; ok {
					t.Fatalf("SpeechSynthesizer must not send language_type: %#v", input)
				}
			} else if _, ok := input["format"]; ok {
				t.Fatalf("Qwen-TTS must not send format: %#v", input)
			} else if input["language_type"] != "English" {
				t.Fatalf("Qwen-TTS language_type = %#v", input["language_type"])
			}
		})
	}
}

func TestPreviewAliyunTTSSynthesizesRequestedTextAndDownloadsOSSOverHTTPS(t *testing.T) {
	controller := NewTTSPreviewController(nil)
	requests := 0
	controller.HTTPClient = &http.Client{Transport: previewRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			if req.Method != http.MethodPost || req.URL.Hostname() != "dashscope.aliyuncs.com" || req.Header.Get("Authorization") != "Bearer test-key" {
				t.Fatalf("unexpected synthesis request: %s %s", req.Method, req.URL)
			}
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			input := body["input"].(map[string]any)
			if input["text"] != "custom preview text" || input["voice"] != "Cherry" {
				t.Fatalf("synthesis input = %#v", input)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"output":{"audio":{"url":"http://dashscope-result-bj.oss-cn-beijing.aliyuncs.com/sample.wav?Expires=123"}}}`)), Request: req}, nil
		}
		if req.Method != http.MethodGet || req.URL.Scheme != "https" || req.URL.Hostname() != "dashscope-result-bj.oss-cn-beijing.aliyuncs.com" || req.URL.RawQuery != "Expires=123" {
			t.Fatalf("unexpected audio request: %s %s", req.Method, req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"audio/wav"}},
			Body:       io.NopCloser(strings.NewReader("RIFFxxxxWAVEaudio")),
			Request:    req,
		}, nil
	})}

	audio, contentType, err := controller.previewAliyunTTS(context.Background(), "test-key", defaultAliyunPreviewAPIURLBeijing, TTSPreviewRequest{
		Model: "qwen3-tts-flash", Voice: "Cherry", Text: "custom preview text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(audio) != "RIFFxxxxWAVEaudio" || contentType != "audio/wav" || requests != 2 {
		t.Fatalf("audio=%q contentType=%q requests=%d", audio, contentType, requests)
	}
}

func TestPreviewRequiresAPIKeyEvenForSystemVoice(t *testing.T) {
	controller := NewTTSPreviewController(nil)
	if _, err := controller.resolvePreviewRequest(TTSPreviewRequest{Provider: "aliyun_qwen", Model: "qwen3-tts-flash", Voice: "Cherry"}, false); err == nil {
		t.Fatal("system voice preview unexpectedly allowed without API key")
	}
}
