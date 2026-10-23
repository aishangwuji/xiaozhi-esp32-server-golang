package database

import (
	"strings"
	"testing"

	"xiaozhi/manager/backend/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestEnsureDefaultRealtimeConfigPreservesCredentials(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:default_realtime?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := ensureDefaultRealtimeConfig(db); err != nil {
		t.Fatal(err)
	}
	var config models.Config
	if err := db.Where("type = ? AND is_default = ?", "realtime", true).First(&config).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(config.JsonData, "api_key") {
		t.Fatal("default config must not contain credentials")
	}
	config.JsonData = `{"api_key":"configured-key"}`
	if err := db.Save(&config).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureDefaultRealtimeConfig(db); err != nil {
		t.Fatal(err)
	}
	var configs []models.Config
	if err := db.Where("type = ?", "realtime").Find(&configs).Error; err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 || configs[0].JsonData != config.JsonData {
		t.Fatalf("default config was duplicated or overwritten: %+v", configs)
	}
}
