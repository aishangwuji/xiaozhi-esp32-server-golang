package controllers

import (
	"testing"

	"xiaozhi/manager/backend/models"
)

func TestVoiceOptionsUseUnsavedModelOverride(t *testing.T) {
	db := previewTestDB(t)
	cfg := models.Config{
		Type: "tts", ConfigID: "aliyun_saved_flash", Provider: "aliyun_qwen", Enabled: true,
		JsonData: `{"model":"qwen3-tts-flash"}`,
	}
	if err := db.Create(&cfg).Error; err != nil {
		t.Fatal(err)
	}

	saved, err := getVoiceOptionsForUser(db, nil, 0, "aliyun_qwen", cfg.ConfigID, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	overridden, err := getVoiceOptionsForUser(db, nil, 0, "aliyun_qwen", cfg.ConfigID, "", "", "qwen-audio-3.0-tts-flash")
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) == 0 || len(overridden) == 0 || saved[0].Value == overridden[0].Value {
		t.Fatalf("saved voices = %#v, override voices = %#v", saved, overridden)
	}
}
