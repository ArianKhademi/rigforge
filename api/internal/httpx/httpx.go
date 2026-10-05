// Package httpx holds the small HTTP helpers shared by every handler package:
// one error envelope, request logging and CORS.
package httpx

import (
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ErrorBody is the single error shape the api returns, so the web client has
// one thing to parse: {"error": {"code": "...", "message": "...", "details": ...}}.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func Error(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": ErrorBody{Code: code, Message: message}})
}

func ErrorWithDetails(c *gin.Context, status int, code, message string, details any) {
	c.AbortWithStatusJSON(status, gin.H{"error": ErrorBody{Code: code, Message: message, Details: details}})
}

// Internal logs the real error and returns an opaque 500, so storage or SQL
// error text never leaks to the client.
func Internal(c *gin.Context, err error) {
	slog.ErrorContext(c.Request.Context(), "internal error",
		"err", err, "path", c.FullPath(), "request_id", c.GetString("request_id"))
	Error(c, http.StatusInternalServerError, "internal", "internal server error")
}

// RequestLogger emits one structured line per request.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		id := c.GetHeader("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		c.Set("request_id", id)
		c.Header("X-Request-Id", id)

		c.Next()

		// FullPath is the route template ("/api/uploads/:id"), which keeps log
		// cardinality bounded; fall back to the raw path for unmatched routes.
		route := c.FullPath()
		if route == "" {
			route = c.Request.URL.Path
		}
		slog.Info("request",
			"method", c.Request.Method,
			"route", route,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"user", c.GetString("user_id"),
			"request_id", id,
		)
	}
}

// CORS allows the configured browser origins. In the default deployment the
// web app and api share an origin (the ingress routes /api), so this only
// matters when the Vite dev server runs on a different port.
func CORS(origins []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && slices.Contains(origins, origin) {
			h := c.Writer.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
