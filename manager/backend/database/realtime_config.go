package database

import (
	"encoding/json"
	"errors"

	"gorm.io/gorm"
	"xiaozhi/manager/backend/models"
)

func ensureDefaultRealtimeConfig(db *gorm.DB) error {
	var count int64
	if err := db.Model(&models.Config{}).Where("type = ? AND is_default = ?", "realtime", true).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return nil
	}
	const id = "realtime_qwen_default"
	var config models.Config
	err := db.Where("type = ? AND config_id = ?", "realtime", id).First(&config).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		data, marshalErr := json.Marshal(map[string]interface{}{
			"base_url":       "wss://dashscope.aliyuncs.com/api-ws/v1/realtime",
			"model":          "qwen-audio-3.1-realtime-plus",
			"voice":          "longanqian_v3.1",
			"turn_detection": "smart_turn",
		})
		if marshalErr != nil {
			return marshalErr
		}
		return db.Create(&models.Config{
			Type: "realtime", Name: "默认 Qwen Realtime 配置", ConfigID: id,
			Provider: "aliyun_qwen_realtime", JsonData: string(data), Enabled: true, IsDefault: true,
		}).Error
	}
	if err != nil {
		return err
	}
	return db.Model(&config).Update("is_default", true).Error
}
