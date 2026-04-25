package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid" // для генерации уникальных ID
	"github.com/joho/godotenv"
)

var jwtSecret = []byte(getEnv("JWT_SECRET", "my-secret-key"))

// Конфигурация MediaMTX (берём из переменных окружения)
var (
	mediamtxHost    = getEnv("MEDIAMTX_HOST", "127.0.0.1")
	mediamtxAPIPort = getEnv("MEDIAMTX_API_PORT", "9997")
	mediamtxSRTPort = getEnv("MEDIAMTX_SRT_PORT", "8890")
	mediamtxHLSPort = getEnv("MEDIAMTX_HLS_PORT", "8888")
	baseStreamPath  = "live" // путь, который мы используем для стримов
)

const (
	testUsername = "operator"
	testPassword = "supersecret"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("Файл .env не найден, используем системные переменные")
	}

	r := gin.Default()

	// CORS middleware (исправленный, разрешает запросы с file:// и обрабатывает OPTIONS)
	r.Use(func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		// Разрешаем null (file://) или любой другой origin
		if origin == "null" || origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
		} else {
			// Для запросов без Origin (например, curl) ставим *
			c.Header("Access-Control-Allow-Origin", "*")
		}
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// Раздача статических файлов (панель) – добавляем перед API
	r.StaticFile("/panel.html", "./static/panel.html") // ← прямой доступ
	r.Static("/static", "./static")                    // ← для дополнительных ресурсов

	api := r.Group("/api/v1")
	{
		api.POST("/auth/login", loginHandler)

		protected := api.Group("", authMiddleware())
		{
			protected.GET("/streams/start", startStreamHandler)
		}
	}

	port := getEnv("PORT", "8080")
	log.Printf("API Gateway запущен на :%s", port)
	log.Printf("Панель доступна по адресу http://localhost:%s/panel.html", port) // ← подсказка
	r.Run(":" + port)
}

func loginHandler(c *gin.Context) {
	var req struct {
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil { // берем по указателю, чтобы ПОМЕНЯТЬ эту структурку
		c.JSON(http.StatusBadRequest, gin.H{"error": "Неверные параметры"})
		return
	}
	if req.Username != testUsername || req.Password != testPassword {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Неверный логин или пароль"})
		return
	}
	claims := jwt.MapClaims{
		"sub": req.Username,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(24 * time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Не удалось создать токен"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"access_token": tokenString,
		"token_type":   "Bearer",
		"expires_in":   86400,
	})
}

// startStreamHandler — создаёт новую трансляцию
func startStreamHandler(c *gin.Context) {
	// Генерируем уникальный ID камеры (можно также принимать из query, если хочешь)
	streamID := uuid.New().String()[:8] // короткий ID, например "a1b2c3d4"

	path := fmt.Sprintf("%s/%s", baseStreamPath, streamID)

	// Проверяем через MediaMTX API, не существует ли уже такой путь
	if pathExists(path) {
		c.JSON(http.StatusConflict, gin.H{"error": "Путь уже используется", "path": path})
		return
	}

	// Формируем URL для SRT-публикации
	srtURL := fmt.Sprintf("srt://%s:%s?streamid=publish:%s", mediamtxHost, mediamtxSRTPort, path)

	// HLS URL для просмотра
	watchURL := fmt.Sprintf("http://%s:%s/%s/index.m3u8", mediamtxHost, mediamtxHLSPort, path)

	c.JSON(http.StatusOK, gin.H{
		"stream_id": streamID,
		"srt_url":   srtURL,
		"watch_url": watchURL,
		"path":      path,
	})
}

// pathExists обращается к MediaMTX API и проверяет, есть ли путь в списке
func pathExists(path string) bool {
	url := fmt.Sprintf("http://%s:%s/v3/paths/list", mediamtxHost, mediamtxAPIPort)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("Ошибка создания запроса к MediaMTX API: %v", err)
		return true
	}

	// Basic Auth: любой логин, пустой пароль
	req.SetBasicAuth("any", "")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("Ошибка запроса к MediaMTX API: %v", err)
		return true
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("MediaMTX API вернул статус: %d", resp.StatusCode)
		return true
	}

	var data struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("Ошибка декодирования ответа MediaMTX: %v", err)
		return true
	}

	for _, item := range data.Items {
		if item.Name == path {
			return true
		}
	}
	return false
}

// authMiddleware (без изменений)
func authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" || len(authHeader) < 7 || authHeader[:7] != "Bearer " {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Токен не предоставлен"})
			return
		}
		tokenString := authHeader[7:]
		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("неверный метод подписи: %v", token.Header["alg"])
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Токен недействителен"})
			return
		}
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Неверные claims"})
			return
		}
		c.Set("claims", claims)
		c.Next()
	}
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

// $headers = @{ Authorization = "Bearer $token" }
// $stream = Invoke-RestMethod -Uri http://localhost:8080/api/v1/streams/start -Method Get -Headers $headers
// $stream | ConvertTo-Json
