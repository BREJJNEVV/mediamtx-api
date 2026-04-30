package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/joho/godotenv"
)

var jwtSecret []byte

// Глобальные переменные (значения будут присвоены в main)
var (
	mediamtxHost    string
	mediamtxAPIPort string
	mediamtxSRTPort string
	mediamtxHLSPort string
	baseStreamPath  string
)

const (
	testUsername = "operator"
	testPassword = "qwe"
)

func main() {
	// Загружаем .env
	if err := godotenv.Load(); err != nil {
		log.Println("Файл .env не найден, используем системные переменные")
	}

	// Присваиваем глобальные переменные ЗДЕСЬ, после загрузки .env
	jwtSecret = []byte(getEnv("JWT_SECRET", "my-secret-key"))
	mediamtxHost = getEnv("MEDIAMTX_HOST", "127.0.0.1")
	mediamtxAPIPort = getEnv("MEDIAMTX_API_PORT", "9997")
	mediamtxSRTPort = getEnv("MEDIAMTX_SRT_PORT", "8890")
	mediamtxHLSPort = getEnv("MEDIAMTX_HLS_PORT", "8888")
	baseStreamPath = "live"

	log.Printf("MEDIAMTX_HOST = %s", mediamtxHost)

	r := gin.Default()

	// CORS
	r.Use(func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin == "null" || origin != "" {
			c.Header("Access-Control-Allow-Origin", origin)
		} else {
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

	r.StaticFile("/panel.html", "./static/panel.html")
	r.Static("/static", "./static")

	api := r.Group("/api/v1")
	{
		api.POST("/auth/login", loginHandler)

		protected := api.Group("", authMiddleware())
		{
			protected.GET("/streams/start", startStreamHandler)
			protected.GET("/streams/status", statusHandler)
		}
	}

	port := getEnv("PORT", "8080")
	log.Printf("API Gateway запущен на :%s", port)
	log.Printf("Панель доступна по адресу http://localhost:%s/panel.html", port)
	r.Run(":" + port)
}

func getMediamtxAPI(path string) ([]byte, error) {
	url := fmt.Sprintf("http://%s:%s%s", mediamtxHost, mediamtxAPIPort, path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("any", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func loginHandler(c *gin.Context) {
	var req struct {
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
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

func startStreamHandler(c *gin.Context) {
	streamID := "cam1" // статический ID
	path := fmt.Sprintf("%s/%s", baseStreamPath, streamID)

	srtURL := fmt.Sprintf("srt://%s:%s?streamid=publish:%s", mediamtxHost, mediamtxSRTPort, path)
	watchURL := fmt.Sprintf("http://%s:%s/%s/index.m3u8", mediamtxHost, mediamtxHLSPort, path)

	c.JSON(http.StatusOK, gin.H{
		"stream_id": streamID,
		"srt_url":   srtURL,
		"watch_url": watchURL,
		"path":      path,
	})
}

func statusHandler(c *gin.Context) {
	// 1. Список путей (расширенная структура)
	pathsBody, err := getMediamtxAPI("/v3/paths/list")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Не удалось получить список путей"})
		return
	}

	var pathsData struct {
		Items []struct {
			Name          string `json:"name"`
			Ready         bool   `json:"ready"`
			BytesReceived int    `json:"bytesReceived"`
			Tracks2       []struct {
				Codec      string `json:"codec"`
				CodecProps struct {
					Width  int `json:"width"`
					Height int `json:"height"`
				} `json:"codecProps,omitempty"`
			} `json:"tracks2"`
			Readers []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"readers"`
		} `json:"items"`
	}
	if err := json.Unmarshal(pathsBody, &pathsData); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка парсинга путей"})
		return
	}

	// 2. SRT‑соединения
	srtBody, err := getMediamtxAPI("/v3/srtconns/list")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Не удалось получить SRT‑соединения"})
		return
	}
	var srtData struct {
		Items []struct {
			ID                  string  `json:"id"`
			Path                string  `json:"path"`
			State               string  `json:"state"`
			RemoteAddr          string  `json:"remoteAddr"`
			PacketsReceivedLoss int     `json:"packetsReceivedLoss"`
			MsRTT               float64 `json:"msRTT"`
			MsReceiveTsbPdDelay int     `json:"msReceiveTsbPdDelay"`
		} `json:"items"`
	}
	if err := json.Unmarshal(srtBody, &srtData); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ошибка парсинга SRT‑соединений"})
		return
	}

	// 3. Собираем расширенную информацию
	type StreamInfo struct {
		Path       string `json:"path"`
		Resolution string `json:"resolution"`
		Viewers    int    `json:"viewers"`
		Bitrate    string `json:"bitrate"`
		Publisher  *struct {
			Address     string `json:"address"`
			State       string `json:"state"`
			PacketsLost int    `json:"packetsLost"`
			RTT         string `json:"rtt"`
			Latency     string `json:"latency"`
		} `json:"publisher,omitempty"`
	}

	var result []StreamInfo
	for _, p := range pathsData.Items {
		info := StreamInfo{
			Path:    p.Name,
			Viewers: len(p.Readers),
		}

		// Разрешение из первой видео-дорожки
		for _, t := range p.Tracks2 {
			if t.Codec == "H264" || t.Codec == "H265" || t.Codec == "VP8" || t.Codec == "VP9" || t.Codec == "AV1" {
				if t.CodecProps.Width > 0 && t.CodecProps.Height > 0 {
					info.Resolution = fmt.Sprintf("%dx%d", t.CodecProps.Width, t.CodecProps.Height)
				}
				break
			}
		}
		if info.Resolution == "" {
			info.Resolution = "-"
		}

		// Приблизительный входящий битрейт
		if p.BytesReceived > 0 {
			// Среднее за последние 5 секунд (при опросе раз в 5 секунд)
			mbps := float64(p.BytesReceived*8) / 5.0 / 1000000.0
			info.Bitrate = fmt.Sprintf("%.2f Mbps", mbps)
		} else {
			info.Bitrate = "-"
		}

		// SRT‑информация
		for _, c := range srtData.Items {
			if c.Path == p.Name && c.State == "publish" {
				info.Publisher = &struct {
					Address     string `json:"address"`
					State       string `json:"state"`
					PacketsLost int    `json:"packetsLost"`
					RTT         string `json:"rtt"`
					Latency     string `json:"latency"`
				}{
					Address:     c.RemoteAddr,
					State:       "active",
					PacketsLost: c.PacketsReceivedLoss,
					RTT:         fmt.Sprintf("%.2f ms", c.MsRTT),
					Latency:     fmt.Sprintf("%d ms", c.MsReceiveTsbPdDelay),
				}
				break
			}
		}
		result = append(result, info)
	}

	c.JSON(http.StatusOK, gin.H{"streams": result})
}
func pathExists(path string) bool {
	body, err := getMediamtxAPI("/v3/paths/list")
	if err != nil {
		log.Printf("Ошибка запроса к MediaMTX API: %v", err)
		return true
	}
	var data struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
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
