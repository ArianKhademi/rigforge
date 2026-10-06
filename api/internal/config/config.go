// Package config loads all runtime configuration from environment variables.
// There are no config files: the same binary runs under docker-compose, in
// tests and on Kubernetes, and only the environment differs.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr        string
	DatabaseURL string
	RedisURL    string
	S3          S3
	Auth        Auth
	Upload      Upload
	CORSOrigins []string
}

type S3 struct {
	// Endpoint is used by the api for its own calls (create/complete/abort).
	// PublicEndpoint is baked into presigned URLs and must be reachable from the
	// browser. On R2 they are identical; with MinIO in compose or kind the api
	// talks to "minio:9000" while the browser needs "localhost:9000".
	Endpoint        string
	PublicEndpoint  string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
}

type Auth struct {
	JWKSURL  string
	Issuer   string
	Audience string
}

type Upload struct {
	PartSize       int64
	MaxSize        int64
	PresignTTL     time.Duration
	IdleTimeout    time.Duration
	ReaperInterval time.Duration
}

const (
	// DefaultPartSize is 64 MiB. R2 requires every part except the last to be
	// the same size and at least 5 MiB; 64 MiB keeps a 2 GiB file at 32 parts
	// and bounds the cost of one lost part to 64 MiB of re-upload.
	DefaultPartSize = 64 << 20
	MinPartSize     = 5 << 20
	// S3 and R2 both cap a multipart upload at 10,000 parts.
	MaxParts = 10000
)

func Load() (Config, error) {
	c := Config{
		Addr:        env("RIGFORGE_ADDR", ":8080"),
		DatabaseURL: env("DATABASE_URL", ""),
		RedisURL:    env("REDIS_URL", "redis://localhost:6379/0"),
		S3: S3{
			Endpoint:        env("S3_ENDPOINT", ""),
			PublicEndpoint:  env("S3_PUBLIC_ENDPOINT", ""),
			Region:          env("S3_REGION", "auto"),
			Bucket:          env("S3_BUCKET", "rigforge-dev"),
			AccessKeyID:     env("S3_ACCESS_KEY_ID", ""),
			SecretAccessKey: env("S3_SECRET_ACCESS_KEY", ""),
		},
		Auth: Auth{
			JWKSURL:  env("JWT_JWKS_URL", ""),
			Issuer:   env("JWT_ISSUER", "rigforge-dev-issuer"),
			Audience: env("JWT_AUDIENCE", "rigforge-api"),
		},
		CORSOrigins: splitList(env("CORS_ORIGINS", "")),
	}
	// A trailing slash (easy to paste from the R2 dashboard) would put a
	// double slash in every object path and break the request signature.
	c.S3.Endpoint = strings.TrimRight(c.S3.Endpoint, "/")
	c.S3.PublicEndpoint = strings.TrimRight(c.S3.PublicEndpoint, "/")
	if c.S3.PublicEndpoint == "" {
		c.S3.PublicEndpoint = c.S3.Endpoint
	}

	var err error
	if c.Upload.PartSize, err = envInt64("UPLOAD_PART_SIZE", DefaultPartSize); err != nil {
		return c, err
	}
	if c.Upload.MaxSize, err = envInt64("UPLOAD_MAX_SIZE", 50<<30); err != nil {
		return c, err
	}
	if c.Upload.PresignTTL, err = envDuration("UPLOAD_PRESIGN_TTL", 15*time.Minute); err != nil {
		return c, err
	}
	if c.Upload.IdleTimeout, err = envDuration("UPLOAD_IDLE_TIMEOUT", 24*time.Hour); err != nil {
		return c, err
	}
	if c.Upload.ReaperInterval, err = envDuration("UPLOAD_REAPER_INTERVAL", 15*time.Minute); err != nil {
		return c, err
	}

	for name, v := range map[string]string{
		"DATABASE_URL":         c.DatabaseURL,
		"S3_ENDPOINT":          c.S3.Endpoint,
		"S3_ACCESS_KEY_ID":     c.S3.AccessKeyID,
		"S3_SECRET_ACCESS_KEY": c.S3.SecretAccessKey,
		"JWT_JWKS_URL":         c.Auth.JWKSURL,
	} {
		if v == "" {
			return c, fmt.Errorf("config: %s is required", name)
		}
	}
	if c.Upload.PartSize < MinPartSize {
		return c, fmt.Errorf("config: UPLOAD_PART_SIZE must be at least %d bytes", MinPartSize)
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return n, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s: %w", key, err)
	}
	return d, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
