package controllers

import (
	"encoding/json"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"xiaozhi/manager/backend/models"
)

func validateRealtimeConfig(provider, raw string) error {
	if provider != "aliyun_qwen_realtime" {
		return fmt.Errorf("暂不支持的 Realtime provider: %s", provider)
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return fmt.Errorf("Realtime JSON 配置无效: %w", err)
	}
	if strings.TrimSpace(configString(data, "api_key")) == "" {
		return fmt.Errorf("Realtime API Key 不能为空")
	}
	turn := configString(data, "turn_detection")
	if turn != "" && turn != "smart_turn" && turn != "server_vad" && turn != "null" {
		return fmt.Errorf("不支持的 turn_detection: %s", turn)
	}
	return nil
}

func configString(data map[string]interface{}, key string) string {
	value, _ := data[key].(string)
	return strings.TrimSpace(value)
}

func validateAgentRealtimeSelection(db *gorm.DB, enabled bool, configID *string) error {
	if !enabled {
		return nil
	}
	query := db.Where("type = ? AND enabled = ?", "realtime", true)
	if configID != nil && strings.TrimSpace(*configID) != "" {
		query = query.Where("config_id = ?", strings.TrimSpace(*configID))
	} else {
		query = query.Where("is_default = ?", true)
	}
	var config models.Config
	if err := query.First(&config).Error; err != nil {
		return fmt.Errorf("请选择可用的 Realtime 配置")
	}
	return validateRealtimeConfig(config.Provider, config.JsonData)
}
