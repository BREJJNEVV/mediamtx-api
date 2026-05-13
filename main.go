package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
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

// ---------- Хранилище метрик SRT (заменяет Prometheus) ----------
type SRTSnapshot struct {
	Timestamp   int64   `json:"timestamp"`
	Path        string  `json:"path"`
	PacketsLoss int     `json:"packets_loss"`
	RTT         float64 `json:"rtt_ms"`
	Bitrate     float64 `json:"bitrate_mbps"`
}

var (
	metricsHistory []SRTSnapshot
	metricsMu      sync.Mutex
	metricsFile    = "metrics_history.json" // для отладки, можно отключить
	previousLoss   = make(map[string]int)   // ключ – путь, значение – последнее кол-во потерянных пакетов
)

// ---------- Телеметрия мобильного устройства (RSSI, GPS) ----------
var telemetryData = struct {
	sync.RWMutex
	RSSI      float64
	Latitude  float64
	Longitude float64
	StreamID  string
}{}

// Сеанс записи метрик
type MetricsRecord struct {
	StartTime time.Time
	Snapshots []SRTSnapshot // наша уже существующая структура
}

var (
	recordingActive bool
	recordingMu     sync.Mutex
	currentRecord   MetricsRecord
)

// Результат статистического анализа
type StatsResult struct {
	Duration   float64          `json:"duration_sec"`
	Count      int              `json:"snapshots_count"`
	OWD        *OWDStats        `json:"owd,omitempty"`
	Jitter     *JitterStats     `json:"jitter,omitempty"`
	PacketLoss *PacketLossStats `json:"packet_loss,omitempty"`
	Bitrate    *BitrateStats    `json:"bitrate,omitempty"`
	RTT        *RTTStats        `json:"rtt,omitempty"`
}

type OWDStats struct {
	Note string `json:"note"` // пояснение, почему пусто
	// поля для будущего заполнения
}

type JitterStats struct {
	Note string `json:"note"`
}

type PacketLossStats struct {
	TotalLossRate float64    `json:"total_loss_rate"` // %
	Burst         *BurstInfo `json:"burst,omitempty"`
	Note          string     `json:"note,omitempty"`
}

type BurstInfo struct {
	Note string `json:"note"` // требуется анализ последовательных потерь
}

type RTTStats struct {
	Min    float64 `json:"min_ms"`
	Max    float64 `json:"max_ms"`
	Mean   float64 `json:"mean_ms"`
	StdDev float64 `json:"std_dev_ms"`
	P50    float64 `json:"p50_ms"`
	P95    float64 `json:"p95_ms"`
	P99    float64 `json:"p99_ms"`
	Hist   []int   `json:"histogram_bins"` // гистограмма (границы корзин можно задать)
}

type BitrateStats struct {
	Min  float64 `json:"min_mbps"`
	Max  float64 `json:"max_mbps"`
	Mean float64 `json:"mean_mbps"`
}

// ---------- Фоновый опрос MediaMTX и сохранение метрик ----------
func pollAndSaveMetrics() {
	for {
		body, err := getMediamtxAPI("/v3/srtconns/list")
		if err != nil {
			log.Printf("Ошибка опроса SRT метрик: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		var srtData struct {
			Items []struct {
				Path                string  `json:"path"`
				State               string  `json:"state"`
				PacketsReceivedLoss int     `json:"packetsReceivedLoss"`
				MsRTT               float64 `json:"msRTT"`
				MbpsReceiveRate     float64 `json:"mbpsReceiveRate"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &srtData); err != nil {
			log.Printf("Ошибка парсинга SRT метрик: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		now := time.Now().Unix()
		metricsMu.Lock()
		for _, item := range srtData.Items {
			if item.State == "publish" {
				// Вычисляем потери за интервал
				currentLoss := item.PacketsReceivedLoss
				intervalLoss := 0
				if prev, ok := previousLoss[item.Path]; ok {
					intervalLoss = currentLoss - prev
					if intervalLoss < 0 {
						intervalLoss = 0
					}
				}
				previousLoss[item.Path] = currentLoss

				// Формируем снимок
				snap := SRTSnapshot{
					Timestamp:   now,
					Path:        item.Path,
					PacketsLoss: intervalLoss,
					RTT:         item.MsRTT,
					Bitrate:     item.MbpsReceiveRate,
				}
				metricsHistory = append(metricsHistory, snap)

				// Если запись активна, добавляем в сеансовую запись
				recordingMu.Lock()
				if recordingActive {
					currentRecord.Snapshots = append(currentRecord.Snapshots, snap)
				}
				recordingMu.Unlock()
			}
		}
		// ... ограничение размера истории и запись в файл
		metricsMu.Unlock()

		time.Sleep(2 * time.Second)
	}
}

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

	// Запускаем фоновый сбор метрик SRT
	go pollAndSaveMetrics()

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
	r.StaticFile("/login.html", "./static/login.html")
	r.StaticFile("/dashboard.html", "./static/dashboard.html")
	r.StaticFile("/video.html", "./static/video.html")
	r.Static("/static", "./static")

	// Удаляем старую ручку /metrics (Prometheus), она теперь не нужна
	// r.GET("/metrics", ...)

	api := r.Group("/api/v1")
	{
		api.POST("/auth/login", loginHandler)

		protected := api.Group("", authMiddleware())
		{
			protected.GET("/streams/start", startStreamHandler)
			protected.GET("/streams/status", statusHandler)
			protected.POST("/telemetry", telemetryHandler)
			protected.GET("/metrics/history", metricsHistoryHandler)
			protected.POST("/metrics/record/start", startRecordingHandler)
			protected.POST("/metrics/record/stop", stopRecordingHandler)
			protected.GET("/metrics/record/result", getRecordingResultHandler)
			protected.GET("/metrics/record/status", func(c *gin.Context) {
				recordingMu.Lock()
				defer recordingMu.Unlock()
				c.JSON(http.StatusOK, gin.H{"active": recordingActive})
			})
		}
	}

	port := getEnv("PORT", "8080")
	log.Printf("API Gateway запущен на :%s", port)
	log.Printf("Панель доступна по адресу http://localhost:%s/login.html", port)
	r.Run(":" + port)
}

// ---------- Вспомогательные функции ----------

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

// ---------- Обработчики API ----------

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

func telemetryHandler(c *gin.Context) {
	var req struct {
		StreamID  string  `json:"stream_id" binding:"required"`
		RSSI      float64 `json:"rssi"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Неверные данные"})
		return
	}
	telemetryData.Lock()
	telemetryData.StreamID = req.StreamID
	telemetryData.RSSI = req.RSSI
	telemetryData.Latitude = req.Latitude
	telemetryData.Longitude = req.Longitude
	telemetryData.Unlock()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Новая ручка для получения истории метрик
func metricsHistoryHandler(c *gin.Context) {
	limitStr := c.DefaultQuery("limit", "50")
	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit <= 0 {
		limit = 50
	}
	metricsMu.Lock()
	defer metricsMu.Unlock()
	if len(metricsHistory) == 0 {
		c.JSON(http.StatusOK, gin.H{"metrics": []SRTSnapshot{}})
		return
	}
	start := len(metricsHistory) - limit
	if start < 0 {
		start = 0
	}
	c.JSON(http.StatusOK, gin.H{"metrics": metricsHistory[start:]})
}

// ---------- Вспомогательные middleware и утилиты ----------

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

func startRecordingHandler(c *gin.Context) {
	recordingMu.Lock()
	defer recordingMu.Unlock()
	if recordingActive {
		c.JSON(http.StatusConflict, gin.H{"error": "Запись уже идёт"})
		return
	}
	// Очищаем предыдущий результат, если был
	currentRecord = MetricsRecord{
		StartTime: time.Now(),
		Snapshots: make([]SRTSnapshot, 0),
	}
	recordingActive = true
	log.Println("Запись метрик начата")
	c.JSON(http.StatusOK, gin.H{"status": "started"})
}

func stopRecordingHandler(c *gin.Context) {
	recordingMu.Lock()
	defer recordingMu.Unlock()
	if !recordingActive {
		c.JSON(http.StatusConflict, gin.H{"error": "Запись не ведётся"})
		return
	}
	recordingActive = false
	log.Printf("Запись метрик остановлена, собрано %d точек", len(currentRecord.Snapshots))
	c.JSON(http.StatusOK, gin.H{"status": "stopped", "points": len(currentRecord.Snapshots)})
}

func getRecordingResultHandler(c *gin.Context) {
	recordingMu.Lock()
	defer recordingMu.Unlock()
	if recordingActive {
		c.JSON(http.StatusConflict, gin.H{"error": "Запись ещё не остановлена"})
		return
	}
	if len(currentRecord.Snapshots) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "Нет данных записи"})
		return
	}
	result := computeStats(currentRecord)
	c.JSON(http.StatusOK, gin.H{"stats": result})
}

func computeStats(rec MetricsRecord) StatsResult {
	snaps := rec.Snapshots
	n := len(snaps)
	duration := snaps[n-1].Timestamp - snaps[0].Timestamp

	// RTT статистики (у нас есть msRTT)
	rtts := make([]float64, n)
	for i, s := range snaps {
		rtts[i] = s.RTT
	}
	sort.Float64s(rtts)

	rttMean := mean(rtts)
	rttStdDev := stdDev(rtts, rttMean)

	// Гистограмма RTT (10 корзин от min до max)
	histBins := 10
	hist := make([]int, histBins)
	if rtts[n-1] > rtts[0] {
		binWidth := (rtts[n-1] - rtts[0]) / float64(histBins)
		for _, v := range rtts {
			idx := int((v - rtts[0]) / binWidth)
			if idx >= histBins {
				idx = histBins - 1
			}
			hist[idx]++
		}
	}

	rttStats := RTTStats{
		Min:    rtts[0],
		Max:    rtts[n-1],
		Mean:   rttMean,
		StdDev: rttStdDev,
		P50:    percentile(rtts, 50),
		P95:    percentile(rtts, 95),
		P99:    percentile(rtts, 99),
		Hist:   hist,
	}

	// Битрейт
	bitrates := make([]float64, n)
	for i, s := range snaps {
		bitrates[i] = s.Bitrate
	}
	bitMin, bitMax := minMax(bitrates)
	bitMean := mean(bitrates)
	bitrateStats := BitrateStats{
		Min:  bitMin,
		Max:  bitMax,
		Mean: bitMean,
	}

	// Потери (общая доля потерянных пакетов за интервал)
	totalLoss := 0
	for _, s := range snaps {
		totalLoss += s.PacketsLoss // это дельта потерь
	}
	//lossRate := 0.0
	// Приблизительно общее количество пакетов можно оценить по битрейту и длительности, но мы не имеем точного числа отправленных пакетов.
	// Поэтому PLR пока оставим нулевым или как "неизвестно".
	plStats := PacketLossStats{
		TotalLossRate: 0,
		Note:          "Точный PLR требует общего количества отправленных пакетов. Сейчас известны только потери за интервалы. Оценка возможна после получения меток отправителя.",
	}

	// OWD и Jitter — заглушки
	owdStats := &OWDStats{Note: "Односторонняя задержка недоступна – требуются временные метки в пакетах. Будет реализовано после добавления SEI-меток в видеопоток."}
	jitterStats := &JitterStats{Note: "Джиттер (IPDV) требует OWD. Сейчас можно оценить вариацию RTT как грубый джиттер."}

	return StatsResult{
		Duration:   float64(duration),
		Count:      n,
		OWD:        owdStats,
		Jitter:     jitterStats,
		PacketLoss: &plStats,
		Bitrate:    &bitrateStats,
		RTT:        &rttStats,
	}
}

// Вспомогательные функции
func mean(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	s := 0.0
	for _, v := range data {
		s += v
	}
	return s / float64(len(data))
}

func stdDev(data []float64, meanVal float64) float64 {
	if len(data) < 2 {
		return 0
	}
	s := 0.0
	for _, v := range data {
		d := v - meanVal
		s += d * d
	}
	return math.Sqrt(s / float64(len(data)-1))
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p / 100.0) * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	if upper >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	if lower == upper {
		return sorted[lower]
	}
	// линейная интерполяция
	frac := idx - float64(lower)
	return sorted[lower] + frac*(sorted[upper]-sorted[lower])
}

func minMax(data []float64) (min, max float64) {
	if len(data) == 0 {
		return 0, 0
	}
	min, max = data[0], data[0]
	for _, v := range data[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return
}
