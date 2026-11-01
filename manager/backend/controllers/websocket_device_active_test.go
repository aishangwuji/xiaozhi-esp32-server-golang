package controllers

import (
	"testing"
	"time"

	"xiaozhi/manager/backend/models"
)

// 回归：设备离线时必须保留/定格最后活跃时间，不得写NULL。
// 曾因离线事件把 last_active_at 置空，导致控制台设备列表全部显示“从未活跃”。
func TestUpdateDeviceLastActive(t *testing.T) {
	db := setupAgentDeviceServiceTestDB(t)
	device := models.Device{
		UserID:     1,
		DeviceCode: "ABC123",
		DeviceName: "F8:1C:28:78:2F:24",
		Activated:  true,
	}
	if err := db.Create(&device).Error; err != nil {
		t.Fatalf("create device: %v", err)
	}

	connectAt := time.Now().Add(-3 * time.Minute)
	rows, err := updateDeviceLastActive(db, device.DeviceName, connectAt)
	if err != nil || rows != 1 {
		t.Fatalf("上线更新失败: rows=%d err=%v", rows, err)
	}

	disconnectAt := time.Now()
	rows, err = updateDeviceLastActive(db, device.DeviceName, disconnectAt)
	if err != nil || rows != 1 {
		t.Fatalf("离线更新失败: rows=%d err=%v", rows, err)
	}

	var got models.Device
	if err := db.Where("device_name = ?", device.DeviceName).First(&got).Error; err != nil {
		t.Fatalf("query device: %v", err)
	}
	if got.LastActiveAt == nil {
		t.Fatalf("离线后 last_active_at 被清空，应定格为离线时刻")
	}
	if diff := got.LastActiveAt.Sub(disconnectAt); diff < -2*time.Second || diff > 2*time.Second {
		t.Fatalf("last_active_at = %v, 期望接近离线时刻 %v", got.LastActiveAt, disconnectAt)
	}
}

// 未知设备：更新应命中 0 行（由调用方返回404），不得报错、不得误写其他设备。
func TestUpdateDeviceLastActiveUnknownDevice(t *testing.T) {
	db := setupAgentDeviceServiceTestDB(t)
	device := models.Device{
		UserID:     1,
		DeviceCode: "XYZ789",
		DeviceName: "AA:BB:CC:DD:EE:FF",
		Activated:  true,
	}
	if err := db.Create(&device).Error; err != nil {
		t.Fatalf("create device: %v", err)
	}

	rows, err := updateDeviceLastActive(db, "00:11:22:33:44:55", time.Now())
	if err != nil {
		t.Fatalf("update unknown device: %v", err)
	}
	if rows != 0 {
		t.Fatalf("未知设备应命中0行, got %d", rows)
	}

	// 测试库为进程内共享内存库（cache=shared），只断言本测试创建的设备不受影响
	var got models.Device
	if err := db.Where("device_name = ?", device.DeviceName).First(&got).Error; err != nil {
		t.Fatalf("query device: %v", err)
	}
	if got.LastActiveAt != nil {
		t.Fatalf("未知设备更新不应影响已有设备, %s 的 last_active_at=%v", got.DeviceName, got.LastActiveAt)
	}
}
