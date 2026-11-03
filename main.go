package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the cloud hosting base image has no zoneinfo
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	// DEV_OPENID makes every header-less request act as one sitter; never allow it next to the production database.
	if os.Getenv("DEV_OPENID") != "" && os.Getenv("MYSQL_ADDRESS") != "" {
		log.Error("DEV_OPENID is for local development and must not be set with MYSQL_ADDRESS")
		os.Exit(1)
	}
	store, err := openStoreFromEnv()
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	travel, _ := strconv.Atoi(env("TRAVEL_MIN", "30"))
	httpClient := &http.Client{Timeout: 60 * time.Second}
	api := &API{
		Store: store,
		Model: &Fallback{
			Models:          modelsFromEnv(httpClient),
			PerTry:          envDuration("AI_TIMEOUT", 25*time.Second),
			RetryWait:       envDuration("AI_RETRY_WAIT", 2*time.Second),
			BreakerFailures: envInt("AI_BREAKER_FAILURES", 3),
			BreakerCooldown: envDuration("AI_BREAKER_COOLDOWN", time.Minute),
			OnError: func(model string, err error) {
				log.Warn("model failed, trying next", "model", model, "err", err)
			},
			OnTrip: func(model string) {
				log.Error("model circuit opened", "model", model)
			},
		},
		// batchdownloadfile first; reading object storage with temporary keys is the fallback.
		Photos: FallbackFetcher{
			&WXFetcher{Env: os.Getenv("WX_ENV"), APIBase: env("WX_API_BASE", "http://api.weixin.qq.com"), HTTP: httpClient},
			&COSFetcher{APIBase: env("WX_API_BASE", "http://api.weixin.qq.com"), Region: env("COS_REGION", "ap-shanghai"), HTTP: httpClient},
		},
		Travel:    time.Duration(travel) * time.Minute,
		DevOpenID: os.Getenv("DEV_OPENID"),
		AILimit:   NewDailyLimit(envInt("AI_DAILY_LIMIT", 60)),
		Limits: map[string]*DailyLimit{
			FeatureSummary: NewDailyLimit(envInt("AI_SUMMARY_DAILY_LIMIT", 20)),
			FeatureIntake:  NewDailyLimit(envInt("AI_INTAKE_DAILY_LIMIT", 100)),
		},
		FeaturesOff: envSet("AI_FEATURES_OFF"),
		Log:         log,
	}
	addr := os.Getenv("HOST") + ":" + env("PORT", "80") // HOST=127.0.0.1 keeps a local dev server off the network
	log.Info("listening", "addr", addr, "models", api.Model.Status(), "features_off", os.Getenv("AI_FEATURES_OFF"))
	if err := http.ListenAndServe(addr, api.Routes()); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// openStoreFromEnv uses the MySQL that WeChat Cloud Hosting injects (MYSQL_ADDRESS etc.)
// and falls back to a local SQLite file for development.
func openStoreFromEnv() (*Store, error) {
	addr := os.Getenv("MYSQL_ADDRESS")
	if addr == "" {
		return OpenStore("sqlite", env("SQLITE_PATH", "petsit.db"))
	}
	creds := os.Getenv("MYSQL_USERNAME") + ":" + os.Getenv("MYSQL_PASSWORD")
	dbName := env("MYSQL_DATABASE", "petsit")
	admin, err := sql.Open("mysql", fmt.Sprintf("%s@tcp(%s)/", creds, addr))
	if err != nil {
		return nil, err
	}
	_, err = admin.Exec("CREATE DATABASE IF NOT EXISTS `" + dbName + "` DEFAULT CHARSET utf8mb4")
	admin.Close()
	if err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	// clientFoundRows: an UPDATE that matches a row but changes nothing must not read as "not found".
	return OpenStore("mysql", fmt.Sprintf("%s@tcp(%s)/%s?charset=utf8mb4&clientFoundRows=true", creds, addr, dbName))
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return def
}

// envSet reads a comma-separated list such as AI_FEATURES_OFF=summary,intake.
func envSet(key string) map[string]bool {
	out := map[string]bool{}
	for _, v := range strings.Split(os.Getenv(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out[v] = true
		}
	}
	return out
}

// Built-in endpoints for the two default model slots; any other slot must set its own.
var modelDefaults = map[string][2]string{
	"AI_PRIMARY":  {"https://api.deepseek.com", "deepseek-flash"},
	"AI_FALLBACK": {"https://dashscope.aliyuncs.com/compatible-mode/v1", "qwen-vl-max"},
}

// modelsFromEnv builds the fallback chain from AI_MODELS (default "AI_PRIMARY,AI_FALLBACK"),
// in that order. Each slot reads <SLOT>_API_KEY, _BASE_URL, _MODEL and _TEXT_ONLY. A slot without
// an API key is skipped, so the service runs (with manual reports only) before any key is set.
func modelsFromEnv(httpClient *http.Client) []LLM {
	var models []LLM
	for _, slot := range strings.Split(env("AI_MODELS", "AI_PRIMARY,AI_FALLBACK"), ",") {
		slot = strings.TrimSpace(slot)
		key := os.Getenv(slot + "_API_KEY")
		if slot == "" || key == "" {
			continue
		}
		def := modelDefaults[slot]
		baseURL, model := env(slot+"_BASE_URL", def[0]), env(slot+"_MODEL", def[1])
		if baseURL == "" || model == "" {
			continue
		}
		models = append(models, &OpenAICompat{
			BaseURL:  baseURL,
			Model:    model,
			APIKey:   key,
			TextOnly: os.Getenv(slot+"_TEXT_ONLY") == "true",
			HTTP:     httpClient,
		})
	}
	return models
}
