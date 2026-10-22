package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"xiaozhi-esp32-server-golang/pkg/aliyuntts"
	"xiaozhi/manager/backend/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type TTSPreviewController struct {
	DB         *gorm.DB
	HTTPClient *http.Client
}

func NewTTSPreviewController(db *gorm.DB) *TTSPreviewController {
	return &TTSPreviewController{
		DB: db,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return errors.New("试听请求重定向次数过多")
				}
				if len(via) == 0 || via[0].Method != http.MethodGet {
					return errors.New("试听 API 不允许重定向")
				}
				normalized, err := normalizeAliyunPreviewAudioURL(req.URL.String())
				if err != nil {
					return err
				}
				req.URL, _ = url.Parse(normalized)
				return nil
			},
		},
	}
}

type TTSPreviewRequest struct {
	Provider     string `json:"provider"`
	ConfigID     string `json:"config_id"`
	Model        string `json:"model"`
	Voice        string `json:"voice"`
	Instruction  string `json:"instruction"`
	LanguageType string `json:"language_type"`
	Text         string `json:"text"`
	APIKey       string `json:"api_key"`
	APIURL       string `json:"api_url"`
	Region       string `json:"region"`
}

const (
	defaultAliyunPreviewAPIURLBeijing   = "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation"
	defaultAliyunPreviewAPIURLSingapore = "https://dashscope-intl.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation"
)

type previewRequestError struct {
	Status  int
	Message string
}

func (e *previewRequestError) Error() string { return e.Message }

// PreviewAdminTTS 管理员在线 TTS 试听
func (tpc *TTSPreviewController) PreviewAdminTTS(c *gin.Context) {
	tpc.PreviewTTS(c, false)
}

// PreviewUserTTS 普通用户在线 TTS 试听
func (tpc *TTSPreviewController) PreviewUserTTS(c *gin.Context) {
	tpc.PreviewTTS(c, true)
}

// PreviewTTS 通用 TTS 试听处理逻辑
func (tpc *TTSPreviewController) PreviewTTS(c *gin.Context, isUserScope bool) {
	var req TTSPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数解析失败: " + err.Error()})
		return
	}

	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		req.Text = "你好，我是小智，这是为您演示的音色与试听效果。"
	}

	resolved, err := tpc.resolvePreviewRequest(req, isUserScope)
	if err != nil {
		status := http.StatusBadRequest
		if requestErr, ok := err.(*previewRequestError); ok {
			status = requestErr.Status
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	var audioBytes []byte
	var contentType string
	audioBytes, contentType, err = tpc.previewAliyunTTS(ctx, resolved.APIKey, resolved.APIURL, resolved)

	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	c.Header("Content-Disposition", "inline; filename=\"tts_preview.wav\"")
	c.Data(http.StatusOK, contentType, audioBytes)
}

func (tpc *TTSPreviewController) resolvePreviewRequest(req TTSPreviewRequest, isUserScope bool) (TTSPreviewRequest, error) {
	if strings.TrimSpace(req.Provider) != "aliyun_qwen" {
		return TTSPreviewRequest{}, &previewRequestError{Status: http.StatusBadRequest, Message: "试听仅支持阿里云百炼 TTS"}
	}
	requestedVoice := strings.TrimSpace(req.Voice)
	requestedInstruction := strings.TrimSpace(req.Instruction)
	requestedText := strings.TrimSpace(req.Text)
	resolved := TTSPreviewRequest{Provider: "aliyun_qwen", ConfigID: strings.TrimSpace(req.ConfigID), Voice: requestedVoice, Instruction: requestedInstruction, Text: requestedText}

	if isUserScope {
		if tpc.DB == nil || resolved.ConfigID == "" {
			return TTSPreviewRequest{}, &previewRequestError{Status: http.StatusBadRequest, Message: "config_id 必填"}
		}
		var cfg models.Config
		if err := tpc.DB.Where("type = ? AND config_id = ? AND provider = ? AND enabled = ?", "tts", resolved.ConfigID, "aliyun_qwen", true).First(&cfg).Error; err != nil {
			return TTSPreviewRequest{}, &previewRequestError{Status: http.StatusNotFound, Message: "未找到可用的阿里云 TTS 配置"}
		}
		applyStoredPreviewConfig(&resolved, cfg.JsonData)
		if requestedVoice != "" {
			resolved.Voice = requestedVoice
		}
		if requestedInstruction != "" {
			resolved.Instruction = requestedInstruction
		}
		if requestedText != "" {
			resolved.Text = requestedText
		}
	} else {
		if tpc.DB != nil && resolved.ConfigID != "" {
			var cfg models.Config
			if err := tpc.DB.Where("type = ? AND config_id = ? AND provider = ?", "tts", resolved.ConfigID, "aliyun_qwen").First(&cfg).Error; err == nil {
				applyStoredPreviewConfig(&resolved, cfg.JsonData)
			}
		}
		applyAdminPreviewOverrides(&resolved, req)
	}

	if resolved.Model == "" {
		resolved.Model = "qwen3-tts-flash"
	}
	if resolved.LanguageType == "" && aliyuntts.GetAliyunModelCapability(resolved.Model).SupportsLanguageType {
		resolved.LanguageType = "Chinese"
	}
	if resolved.Voice == "" {
		resolved.Voice = "Cherry"
	}
	if resolved.APIURL == "" {
		if strings.EqualFold(resolved.Region, "singapore") {
			resolved.APIURL = defaultAliyunPreviewAPIURLSingapore
		} else {
			resolved.APIURL = defaultAliyunPreviewAPIURLBeijing
		}
	}
	resolvedAPIURL, err := aliyuntts.ResolveHTTPAPIURL(resolved.APIURL, resolved.Model)
	if err != nil {
		return TTSPreviewRequest{}, &previewRequestError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	resolved.APIURL = resolvedAPIURL
	if strings.TrimSpace(resolved.APIKey) == "" {
		return TTSPreviewRequest{}, &previewRequestError{Status: http.StatusBadRequest, Message: "未配置有效的 API Key，无法试听"}
	}
	if err := validateAliyunPreviewURL(resolved.APIURL, false); err != nil {
		return TTSPreviewRequest{}, &previewRequestError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	return resolved, nil
}

func applyStoredPreviewConfig(req *TTSPreviewRequest, raw string) {
	var data map[string]any
	if json.Unmarshal([]byte(raw), &data) != nil {
		return
	}
	req.APIKey = strings.TrimSpace(getStringAny(data, "api_key"))
	req.APIURL = strings.TrimSpace(getStringAny(data, "api_url"))
	req.Region = strings.TrimSpace(getStringAny(data, "region"))
	req.Model = strings.TrimSpace(getStringAny(data, "model"))
	req.Voice = strings.TrimSpace(getStringAny(data, "voice"))
	req.Instruction = strings.TrimSpace(getStringAny(data, "voice_prompt", "instructions"))
	req.LanguageType = strings.TrimSpace(getStringAny(data, "language_type"))
	if req.APIKey == "" {
		req.APIKey = firstPreviewAPIKey(data["api_keys"])
	}
}

func firstPreviewAPIKey(value any) string {
	rows, _ := value.([]any)
	for _, row := range rows {
		item, _ := row.(map[string]any)
		if enabled, ok := item["enabled"].(bool); ok && !enabled {
			continue
		}
		if key := strings.TrimSpace(getStringAny(item, "api_key")); key != "" {
			return key
		}
	}
	return ""
}

func applyAdminPreviewOverrides(dst *TTSPreviewRequest, src TTSPreviewRequest) {
	if value := strings.TrimSpace(src.APIKey); value != "" {
		dst.APIKey = value
	}
	if value := strings.TrimSpace(src.APIURL); value != "" {
		dst.APIURL = value
	}
	if value := strings.TrimSpace(src.Region); value != "" {
		dst.Region = value
	}
	if value := strings.TrimSpace(src.Model); value != "" {
		dst.Model = value
	}
	if value := strings.TrimSpace(src.Voice); value != "" {
		dst.Voice = value
	}
	if value := strings.TrimSpace(src.Instruction); value != "" {
		dst.Instruction = value
	}
	if value := strings.TrimSpace(src.LanguageType); value != "" {
		dst.LanguageType = value
	}
	if value := strings.TrimSpace(src.Text); value != "" {
		dst.Text = value
	}
}

func validateAliyunPreviewURL(raw string, audioURL bool) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" || parsed.Port() != "" || parsed.Fragment != "" {
		return errors.New("试听地址必须是合法的 HTTPS URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
		return errors.New("试听地址不能指向内网")
	}
	if audioURL {
		if !strings.HasSuffix(host, ".aliyuncs.com") {
			return errors.New("试听音频地址不是允许的阿里云 OSS 域名")
		}
		labels := strings.Split(strings.TrimSuffix(host, ".aliyuncs.com"), ".")
		ossLabel := labels[len(labels)-1]
		if len(labels) > 2 || !strings.HasPrefix(ossLabel, "oss-") || (len(labels) == 2 && labels[0] == "") {
			return errors.New("试听音频地址不是允许的阿里云域名")
		}
		return nil
	}
	if host != "dashscope.aliyuncs.com" && host != "dashscope-intl.aliyuncs.com" && !strings.HasSuffix(host, ".maas.aliyuncs.com") {
		return errors.New("试听 API 地址不是允许的 DashScope 域名")
	}
	return nil
}

func normalizeAliyunPreviewAudioURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("试听音频地址必须是合法的 HTTP(S) URL")
	}
	parsed.Scheme = "https"
	if err := validateAliyunPreviewURL(parsed.String(), true); err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func (tpc *TTSPreviewController) previewAliyunTTS(ctx context.Context, apiKey, apiURL string, req TTSPreviewRequest) ([]byte, string, error) {
	if apiURL == "" {
		apiURL = defaultAliyunPreviewAPIURLBeijing
	}
	resolvedAPIURL, err := aliyuntts.ResolveHTTPAPIURL(apiURL, req.Model)
	if err != nil {
		return nil, "", err
	}
	apiURL = resolvedAPIURL
	reqBody := buildAliyunPreviewRequestBody(req)
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, "", fmt.Errorf("构建阿里云试听请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, "", fmt.Errorf("创建阿里云试听请求失败: %w", err)
	}
	httpReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := tpc.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("调用阿里云试听失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, "", fmt.Errorf("读取阿里云试听响应失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("阿里云试听 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	parsed, err := unmarshalJSONMap(respBody)
	if err != nil {
		return nil, "", fmt.Errorf("解析阿里云试听响应失败: %w", err)
	}

	output, ok := parsed["output"].(map[string]any)
	if !ok {
		return nil, "", errors.New("阿里云试听响应缺少 output")
	}
	audioOutput, ok := output["audio"].(map[string]any)
	if !ok {
		return nil, "", errors.New("阿里云试听响应缺少 output.audio")
	}
	audioURL := strings.TrimSpace(getStringAny(audioOutput, "url"))
	if audioURL == "" {
		return nil, "", errors.New("阿里云试听响应缺少 output.audio.url")
	}
	return tpc.downloadPreviewAudio(ctx, audioURL)
}

func (tpc *TTSPreviewController) downloadPreviewAudio(ctx context.Context, audioURL string) ([]byte, string, error) {
	normalizedURL, err := normalizeAliyunPreviewAudioURL(audioURL)
	if err != nil {
		return nil, "", err
	}
	audioReq, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizedURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("创建试听音频下载请求失败: %w", err)
	}
	audioResp, err := tpc.HTTPClient.Do(audioReq)
	if err != nil {
		return nil, "", fmt.Errorf("下载试听音频失败: %w", err)
	}
	defer audioResp.Body.Close()

	audioBytes, err := io.ReadAll(io.LimitReader(audioResp.Body, 20*1024*1024+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取试听音频失败: %w", err)
	}
	if audioResp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("下载试听音频 HTTP %d", audioResp.StatusCode)
	}
	if len(audioBytes) == 0 {
		return nil, "", errors.New("试听返回音频为空")
	}
	if len(audioBytes) > 20*1024*1024 {
		return nil, "", errors.New("试听音频超过 20MB 限制")
	}
	if len(audioBytes) < 12 || !bytes.HasPrefix(audioBytes, []byte("RIFF")) || !bytes.Equal(audioBytes[8:12], []byte("WAVE")) {
		return nil, "", errors.New("试听返回的音频不是 WAV 格式")
	}
	return audioBytes, "audio/wav", nil
}

func buildAliyunPreviewRequestBody(req TTSPreviewRequest) map[string]any {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "qwen3-tts-flash"
	}
	voice := strings.TrimSpace(req.Voice)
	if voice == "" {
		voice = "Cherry"
	}
	instruction := strings.TrimSpace(req.Instruction)
	capability := aliyuntts.GetAliyunModelCapability(model)
	if instruction != "" && model == "qwen3-tts-flash" {
		model = "qwen3-tts-instruct-flash"
		capability = aliyuntts.GetAliyunModelCapability(model)
	}

	inputMap := map[string]any{"text": req.Text, "voice": voice}
	if capability.Category == aliyuntts.CategoryQwenAudio || capability.Category == aliyuntts.CategoryCosyVoice {
		if instruction != "" && capability.SupportsInstruction {
			inputMap["instruction"] = instruction
		}
		// 试听统一请求 WAV，与浏览器返回的音频容器一致；不沿用设备播放格式。
		inputMap["format"] = "wav"
		inputMap["sample_rate"] = capability.DefaultSampleRate
	} else {
		if instruction != "" && capability.SupportsInstruction {
			inputMap["instructions"] = instruction
			inputMap["optimize_instructions"] = true
		}
		if languageType := strings.TrimSpace(req.LanguageType); languageType != "" {
			inputMap["language_type"] = languageType
		}
	}
	return map[string]any{"model": model, "input": inputMap}
}
