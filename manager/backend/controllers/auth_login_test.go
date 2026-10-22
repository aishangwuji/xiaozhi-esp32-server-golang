package controllers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"xiaozhi/manager/backend/middleware"
	"xiaozhi/manager/backend/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func TestLoginDoesNotAcceptHardcodedAdmin(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:pr281-login?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.Config{
		Type: "auth", ConfigID: "auth_test", JsonData: `{"login_captcha_enabled":false}`, IsDefault: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("unique-private-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.User{
		Username: "admin", Email: "admin@example.invalid", Password: string(passwordHash), Role: "admin",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := middleware.ConfigureJWT("test-only-private-signing-secret", 24); err != nil {
		t.Fatal(err)
	}
	controller := &AuthController{DB: db}
	for _, test := range []struct {
		password string
		status   int
	}{
		{password: "admin123", status: http.StatusUnauthorized},
		{password: "unique-private-password", status: http.StatusOK},
	} {
		payload, _ := json.Marshal(map[string]string{"username": "admin", "password": test.password})
		response := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(response)
		context.Request = httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(payload))
		context.Request.Header.Set("Content-Type", "application/json")
		controller.Login(context)
		if response.Code != test.status {
			t.Errorf("password %q: status = %d, want %d", test.password, response.Code, test.status)
		}
	}
}
