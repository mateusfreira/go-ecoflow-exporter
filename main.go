package main

import (
	"context"
	"errors"
	"github.com/go-redis/redis/v8"
	"github.com/tess1o/go-ecoflow"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// generic
const (
	defaultMetricPrefix = "ecoflow"
	defaultInterval     = 30
	defaultExporterType = "rest"
)

// prometheus
const (
	defaultMetricsPort             = "2112"
	defaultOfflineThresholdSeconds = 60
)

// timescaledb
const (
	timescaleDbSource = "file://migrations/timescale"
)

// redis
const (
	defaultRedisUrl = "localhost:6379"
	defaultRedisDb  = 0
)

//errors

var (
	accessTokenMandatoryErr   = errors.New("ECOFLOW_ACCESS_KEY and ECOFLOW_SECRET_KEY are mandatory")
	emailPasswordMandatoryErr = errors.New("ECOFLOW_EMAIL and ECOFLOW_PASSWORD and ECOFLOW_DEVICES are mandatory")
)

type Shutdownable interface {
	Close(ctx context.Context)
}

func main() {
	setLoggerLevel()

	slog.Info("Starting the exporter...")
	metricPrefix := getStringOrDefault("METRIC_PREFIX", defaultMetricPrefix)

	var handlers []MetricHandler
	handlers = enablePrometheus(metricPrefix, handlers)
	handlers = enableTimescaleDb(metricPrefix, handlers)
	handlers = enableRedis(metricPrefix, handlers)

	if len(handlers) == 0 {
		slog.Error("No metric handlers enabled. Make sure at least one metric handle is enabled: " +
			"PROMETHEUS_ENABLED=true or TIMESCALE_ENABLED=true or REDIS_ENABLED=true")
		os.Exit(1)
	}

	done := make(chan bool, 1)

	setupGracefulShutdown(handlers, done)

	exporterType := getStringOrDefault("EXPORTER_TYPE", defaultExporterType)
	switch exporterType {
	case "rest":
		err := createAndStartRestExporter(handlers)
		if err != nil {
			slog.Error("Unable to start rest exporter", "error", err)
			return
		}
	case "mqtt":
		err := createAndStartMqttExporter(handlers)
		if err != nil {
			slog.Error("Unable to start mqtt exporter", "error", err)
			return
		}
	default:
		slog.Error("Unknown exporter type. Supported types: rest, mqtt")
		return
	}

	<-done
	slog.Info("Application has been stopped")
}

func createAndStartMqttExporter(handlers []MetricHandler) error {
	email := os.Getenv("ECOFLOW_EMAIL")
	password := os.Getenv("ECOFLOW_PASSWORD")
	devices := os.Getenv("ECOFLOW_DEVICES")
	if email == "" || password == "" || devices == "" {
		return emailPasswordMandatoryErr
	}

	devicesList := strings.Split(devices, ",")

	offlineThreshold := getIntOrDefault("MQTT_DEVICE_OFFLINE_THRESHOLD_SECONDS", defaultOfflineThresholdSeconds)

	exporter, err := NewMqttMetricsExporter(email, password, devicesList, time.Second*time.Duration(offlineThreshold), handlers...)

	if err != nil {
		return err
	}

	go exporter.ExportMetrics()
	return nil
}

func createAndStartRestExporter(handlers []MetricHandler) error {
	interval := getIntOrDefault("SCRAPING_INTERVAL", defaultInterval)

	accessKey := os.Getenv("ECOFLOW_ACCESS_KEY")
	secretKey := os.Getenv("ECOFLOW_SECRET_KEY")

	if accessKey == "" || secretKey == "" {
		return accessTokenMandatoryErr
	}
	client := ecoflow.NewEcoflowClient(accessKey, secretKey)

	exporter := NewMetricsExporter(client, time.Duration(interval)*time.Second, handlers...)
	go exporter.ExportMetrics()
	return nil
}

func enableTimescaleDb(metricPrefix string, handlers []MetricHandler) []MetricHandler {
	if isOptionEnabled("TIMESCALE_ENABLED") {
		timescaleUrl := os.Getenv("TIMESCALE_URL")
		if timescaleUrl == "" {
			log.Fatal("TIMESCALE_URL is mandatory if TIMESCALE_ENABLED is true")
		}

		timescaleExporter := NewTimescaleExporter(&TimescaleExporterConfig{
			Prefix:             metricPrefix,
			TimescaleUrl:       timescaleUrl,
			MigrationSourceUrl: timescaleDbSource,
		})

		handlers = append(handlers, timescaleExporter)
	}
	return handlers
}

func enableRedis(prefix string, handlers []MetricHandler) []MetricHandler {
	if isOptionEnabled("REDIS_ENABLED") {
		config := &redis.Options{
			Addr: getStringOrDefault("REDIS_URL", defaultRedisUrl),
			DB:   getIntOrDefault("REDIS_DB", defaultRedisDb),
		}

		redisUser, exists := os.LookupEnv("REDIS_USER")
		if exists {
			config.Username = redisUser
		}
		redisPassword, exists := os.LookupEnv("REDIS_PASSWORD")
		if exists {
			config.Password = redisPassword
		}

		redisExporter := NewRedisExporter(&RedisExporterConfig{
			Prefix:      prefix,
			RedisConfig: config,
		})
		handlers = append(handlers, redisExporter)
	}

	return handlers
}

func enablePrometheus(metricPrefix string, handlers []MetricHandler) []MetricHandler {
	if isOptionEnabled("PROMETHEUS_ENABLED") {
		port := getStringOrDefault("PROMETHEUS_PORT", defaultMetricsPort)
		promExp := NewPrometheusExporter(&PrometheusConfig{
			Prefix:     metricPrefix,
			ServerPort: port,
		})
		handlers = append(handlers, promExp)
	}
	return handlers
}

func setupGracefulShutdown(handlers []MetricHandler, done chan bool) {
	// Create a channel to listen for OS signals
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	// Goroutine to handle shutdown
	go func() {
		<-signalChan
		slog.Info("Received shutdown signal")

		// Create a context with a timeout to allow outstanding requests to complete
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		for _, shutdownable := range handlers {
			s, ok := shutdownable.(Shutdownable)
			if ok {
				s.Close(ctx)
			}
		}
		done <- true
	}()
}

func setLoggerLevel() {
	debugEnabled := os.Getenv("DEBUG_ENABLED")
	if debugEnabled == "true" || debugEnabled == "1" {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	} else {
		slog.SetLogLoggerLevel(slog.LevelInfo)
	}
}

func getStringOrDefault(key, def string) string {
	val, exists := os.LookupEnv(key)
	if exists {
		return val
	}
	return def
}

func getIntOrDefault(key string, def int) int {
	val, exists := os.LookupEnv(key)
	if exists {
		intVal, err := strconv.Atoi(val)
		if err != nil {
			return def
		}
		return intVal
	}
	return def
}

func isOptionEnabled(key string) bool {
	val, exists := os.LookupEnv(key)
	if exists && (val == "1" || val == "true") {
		return true
	}
	return false
}
