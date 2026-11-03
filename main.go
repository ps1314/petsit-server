package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
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
	aiLimit, _ := strconv.Atoi(env("AI_DAILY_LIMIT", "60"))
	httpClient := &http.Client{Timeout: 60 * time.Second}
	api := &API{
		Store: store,
		Model: &Fallback{
			Models:    modelsFromEnv(httpClient),
			PerTry:    25 * time.Second,
			RetryWait: 2 * time.Second,
			OnError: func(model string, err error) {
				log.Warn("model failed, trying next", "model", model, "err", err)
			},
		},
		// batchdownloadfile first; reading object storage with temporary keys is the fallback.
		Photos: FallbackFetcher{
			&WXFetcher{Env: os.Getenv("WX_ENV"), APIBase: env("WX_API_BASE", "http://api.weixin.qq.com"), HTTP: httpClient},
			&COSFetcher{APIBase: env("WX_API_BASE", "http://api.weixin.qq.com"), Region: env("COS_REGION", "ap-shanghai"), HTTP: httpClient},
		},
		Travel:    time.Duration(travel) * time.Minute,
		DevOpenID: os.Getenv("DEV_OPENID"),
		AILimit:   NewDailyLimit(aiLimit),
		Log:       log,
	}
	addr := os.Getenv("HOST") + ":" + env("PORT", "80") // HOST=127.0.0.1 keeps a local dev server off the network
	log.Info("listening", "addr", addr, "models", len(api.Model.Models))
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

// modelsFromEnv builds the fallback chain; a model without an API key is skipped, so the
// service runs (with manual reports only) before any key is configured.
func modelsFromEnv(httpClient *http.Client) []ReportModel {
	var models []ReportModel
	for _, prefix := range []string{"AI_PRIMARY", "AI_FALLBACK"} {
		key := os.Getenv(prefix + "_API_KEY")
		if key == "" {
			continue
		}
		baseURL, model := "https://api.deepseek.com", "deepseek-flash"
		if prefix == "AI_FALLBACK" {
			baseURL, model = "https://dashscope.aliyuncs.com/compatible-mode/v1", "qwen-vl-max"
		}
		models = append(models, &OpenAICompat{
			BaseURL: env(prefix+"_BASE_URL", baseURL),
			Model:   env(prefix+"_MODEL", model),
			APIKey:  key,
			HTTP:    httpClient,
		})
	}
	return models
}
