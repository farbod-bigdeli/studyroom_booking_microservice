package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func main() {
	port := getenv("PORT", "8081")
	uri := getenv("MONGODB_URI", "mongodb://root:example@mongo:27017/?authSource=admin")
	dbName := getenv("MONGODB_DB", "auth_db")
	secret := getenv("JWT_SECRET", "dev-secret-change")
	issuer := getenv("JWT_ISSUER", "studyroom")
	ttl := getenvDur("JWT_TTL", 150*time.Minute)

	ctx := context.Background()
	mc, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil { log.Fatal(err) }
	db := mc.Database(dbName)
	users := db.Collection("users")
	ensureIndexes(ctx, users)

	r := gin.Default()

	type creds struct{ Email, Password string }
	r.POST("/register", func(c *gin.Context) {
		var in creds
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error":"invalid json"}); return
		}
		email := strings.ToLower(strings.TrimSpace(in.Email))
		if !strings.Contains(email,"@") || len(in.Password) < 8 {
			c.JSON(http.StatusBadRequest, gin.H{"error":"invalid email/password"}); return
		}
		hash, _ := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		_, err := users.InsertOne(ctx, bson.M{"email":email, "password_hash":hash, "is_admin": false})
		if writeConflict(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error":"email exists"}); return
		}
		c.JSON(http.StatusCreated, gin.H{"status":"registered"})
	})

	r.POST("/login", func(c *gin.Context) {
		var in creds
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error":"invalid json"}); return
		}
		email := strings.ToLower(strings.TrimSpace(in.Email))
		var u struct {
			ID primitive.ObjectID `bson:"_id"`
			Email string `bson:"email"`
			Hash []byte `bson:"password_hash"`
			Admin bool  `bson:"is_admin"`
		}
		if err := users.FindOne(ctx, bson.M{"email": email}).Decode(&u); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error":"invalid credentials"}); return
		}
		if bcrypt.CompareHashAndPassword(u.Hash, []byte(in.Password)) != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error":"invalid credentials"}); return
		}
		// issue JWT
		claims := jwt.MapClaims{
			"iss": issuer,
			"sub": u.ID.Hex(),
			"email": u.Email,
			"is_admin": u.Admin,
			"exp": time.Now().Add(ttl).Unix(),
			"iat": time.Now().Unix(),
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		signed, _ := tok.SignedString([]byte(secret))
		c.JSON(http.StatusOK, gin.H{"access_token": signed})
	})

	r.GET("/me", func(c *gin.Context) {
		// Gateway already verified and put headers; here we re-check if needed (optional)
		c.JSON(http.StatusOK, gin.H{
			"id": c.GetHeader("X-User-Id"),
			"email": c.GetHeader("X-User-Email"),
			"is_admin": c.GetHeader("X-User-Admin") == "true",
		})
	})

	log.Println("auth-svc :" + port)
	_ = r.Run(":" + port)
}

func ensureIndexes(ctx context.Context, col *mongo.Collection) {
	_, _ = col.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key:"email", Value:1}},
		Options: options.Index().SetUnique(true),
	})
}
func writeConflict(err error) bool {
	if err == nil { return false }
	if we, ok := err.(mongo.WriteException); ok {
		for _, e := range we.WriteErrors { if e.Code == 11000 { return true } }
	}
	return false
}
func getenv(k, def string) string { if v := os.Getenv(k); v != "" { return v }; return def }
func getenvDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" { d, err := time.ParseDuration(v); if err == nil { return d } }
	return def
}
