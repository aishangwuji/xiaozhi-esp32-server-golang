package doubaoapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

func NewRequestID() string {
	return uuid.NewString()
}

func NewConnectID() string {
	return uuid.NewString()
}

func NewASRHeaders(appKey, accessKey, resourceID, connectID string) http.Header {
	reqID := NewRequestID()
	if connectID == "" {
		connectID = reqID
	}
	header := http.Header{}
	header.Set("X-Api-Resource-Id", resourceID)
	header.Set("X-Api-Connect-Id", connectID)
	header.Set("X-Api-Request-Id", reqID)
	header.Set("X-Api-Access-Key", accessKey)
	header.Set("X-Api-App-Key", appKey)
	return header
}

// NewTTSHeaders 构造豆包 v3 TTS 的鉴权头。
//
// 火山有两套鉴权方式，按是否配置 apiKey 自动选择：
//   - 新版控制台：只发 X-Api-Key（API Key 管理页拿到的 UUID，推荐）
//   - 旧版控制台：X-Api-App-Id + X-Api-Access-Key（应用管理页的 APP ID / Access Token）
//
// 两套方式互斥，混着发会 401。
func NewTTSHeaders(appID, accessKey, apiKey, resourceID string) http.Header {
	header := http.Header{}
	header.Set("X-Api-Resource-Id", resourceID)
	header.Set("X-Api-Request-Id", NewRequestID())

	if key := strings.TrimSpace(apiKey); key != "" {
		header.Set("X-Api-Key", key)
		return header
	}

	header.Set("Authorization", fmt.Sprintf("Bearer;%s", accessKey))
	header.Set("X-Api-App-Id", appID)
	header.Set("X-Api-Access-Key", accessKey)
	return header
}

func NewTTSWebsocketHeaders(appID, accessKey, apiKey, resourceID, connectID string) http.Header {
	header := NewTTSHeaders(appID, accessKey, apiKey, resourceID)
	if connectID != "" {
		header.Set("X-Api-Connect-Id", connectID)
	}
	return header
}
