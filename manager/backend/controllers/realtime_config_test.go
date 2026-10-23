package controllers

import (
	"encoding/json"
	"strings"
	"testing"

	"xiaozhi/manager/backend/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestValidateRealtimeConfig(t *testing.T) {
	valid := `{"api_key":"test-key","turn_detection":"smart_turn"}`
	if err := validateRealtimeConfig("aliyun_qwen_realtime", valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, test := range []struct{ provider, raw string }{
		{"gemini_live", valid},
		{"aliyun_qwen_realtime", `{"turn_detection":"smart_turn"}`},
		{"aliyun_qwen_realtime", `{"api_key":"test-key","turn_detection":"invalid"}`},
		{"aliyun_qwen_realtime", `not json`},
	} {
		if err := validateRealtimeConfig(test.provider, test.raw); err == nil {
			t.Fatalf("accepted invalid config: provider=%s raw=%s", test.provider, test.raw)
		}
	}
}

func TestValidateAgentRealtimeSelection(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:realtime_selection?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := validateAgentRealtimeSelection(db, false, nil); err != nil {
		t.Fatalf("disabled agent rejected: %v", err)
	}
	if err := validateAgentRealtimeSelection(db, true, nil); err == nil {
		t.Fatal("enabled agent accepted without a default config")
	}
	config := models.Config{Type: "realtime", ConfigID: "qwen", Provider: "aliyun_qwen_realtime", Enabled: true, IsDefault: true, JsonData: `{"api_key":"test-key"}`}
	if err := db.Create(&config).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateAgentRealtimeSelection(db, true, nil); err != nil {
		t.Fatalf("enabled agent rejected: %v", err)
	}
	missing := "missing"
	if err := validateAgentRealtimeSelection(db, true, &missing); err == nil {
		t.Fatal("enabled agent accepted a missing selection")
	}
	if err := db.Model(&config).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateAgentRealtimeSelection(db, true, nil); err == nil {
		t.Fatal("enabled agent accepted a disabled config")
	}
	public, err := json.Marshal(toUserConfigResponse(&config))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "test-key") || strings.Contains(string(public), "json_data") {
		t.Fatal("user response leaked API key")
	}
}
