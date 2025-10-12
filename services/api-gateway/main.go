package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

func main() {
	port := getenv("PORT", "8080")
	jwtSecret := getenv("JWT_SECRET", "dev-secret-change")
	authURL := getenv("AUTH_URL", "http://auth-svc:8081")
	roomsURL := getenv("ROOMS_URL", "http://rooms-svc:8082")
	bookURL := getenv("BOOKINGS_URL", "http://bookings-svc:8083")

	r := gin.Default()

	// Auth passthrough
	r.POST("/register", proxyJSON(authURL+"/register"))
	r.POST("/login", proxyJSON(authURL+"/login"))

	// Me requires JWT
	auth := r.Group("/", authRequired(jwtSecret))
	auth.GET("/me", proxyGET(authURL+"/me"))

	// Admin routes
	admin := r.Group("/admin", authRequired(jwtSecret), mustAdmin())
	admin.POST("/rooms", proxyJSON(roomsURL+"/rooms"))
	admin.GET("/rooms", proxyGET(roomsURL+"/rooms"))
	admin.POST("/rooms/:id/schedules", proxyJSONPath(roomsURL+"/rooms/:id/schedules"))

	// User routes
	user := r.Group("/", authRequired(jwtSecret))
	user.POST("/bookings", proxyJSONWithInternal(bookURL+"/bookings"))
	user.DELETE("/bookings/:id", proxyDELETEWithInternal(bookURL+"/bookings/:id"))
	user.POST("/waitlist", proxyJSONWithInternal(bookURL+"/waitlist"))
	user.GET("/search", proxyGETWithInternal(bookURL+"/search"))

	log.Println("api-gateway :" + port)
	_ = r.Run(":" + port)
}

// --- Middleware ---

func authRequired(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authz := c.GetHeader("Authorization")
		if !strings.HasPrefix(authz, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		tokenStr := strings.TrimPrefix(authz, "Bearer ")
		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) { return []byte(secret), nil })
		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid claims"})
			return
		}
		// propagate user to downstream services via headers
		setUserHeadersFromClaims(c, claims)
		c.Next()
	}
}

func mustAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("X-User-Admin") != "true" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin only"})
			return
		}
		c.Next()
	}
}

func setUserHeadersFromClaims(c *gin.Context, claims jwt.MapClaims) {
	if v, ok := claims["sub"].(string); ok { c.Request.Header.Set("X-User-Id", v) }
	if v, ok := claims["email"].(string); ok { c.Request.Header.Set("X-User-Email", v) }
	if v, ok := claims["is_admin"].(bool); ok && v { c.Request.Header.Set("X-User-Admin", "true") } else { c.Request.Header.Set("X-User-Admin", "false") }
	// internal S2S shared secret
	c.Request.Header.Set("X-Internal-Token", "internal-call-secret")
}

// --- Proxy helpers (very small reverse proxy) ---

func proxyJSON(url string) gin.HandlerFunc {
	return func(c *gin.Context) { httpProxy(c, "POST", url, false) }
}
func proxyGET(url string) gin.HandlerFunc {
	return func(c *gin.Context) { httpProxy(c, "GET", url, false) }
}
func proxyJSONPath(url string) gin.HandlerFunc {
	return func(c *gin.Context) { httpProxy(c, "POST", strings.ReplaceAll(url, ":id", c.Param("id")), false) }
}

func proxyGETWithInternal(url string) gin.HandlerFunc {
	return func(c *gin.Context) { httpProxy(c, "GET", url, true) }
}
func proxyJSONWithInternal(url string) gin.HandlerFunc {
	return func(c *gin.Context) { httpProxy(c, "POST", url, true) }
}
func proxyDELETEWithInternal(url string) gin.HandlerFunc {
	return func(c *gin.Context) { httpProxy(c, "DELETE", strings.ReplaceAll(url, ":id", c.Param("id")), true) }
}

func httpProxy(c *gin.Context, method, url string, passAllHeaders bool) {
	var body io.Reader
	if method == "POST" || method == "PUT" || method == "PATCH" {
		b, _ := io.ReadAll(c.Request.Body)
		c.Request.Body.Close()
		body = bytes.NewReader(b)
	} else {
		body = nil
	}
	req, _ := http.NewRequest(method, url+"?"+c.Request.URL.RawQuery, body)
	req.Header.Set("Content-Type", "application/json")
	// propagate auth-derived headers
	req.Header.Set("X-User-Id", c.GetHeader("X-User-Id"))
	req.Header.Set("X-User-Email", c.GetHeader("X-User-Email"))
	req.Header.Set("X-User-Admin", c.GetHeader("X-User-Admin"))
	req.Header.Set("X-Internal-Token", c.GetHeader("X-Internal-Token"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	c.Status(resp.StatusCode)
	io.Copy(c.Writer, resp.Body)
}

func getenv(k, def string) string { if v := os.Getenv(k); v != "" { return v }; return def }
