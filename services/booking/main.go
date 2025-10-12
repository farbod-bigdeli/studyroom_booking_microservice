package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	roomspb "bookings-svc/internal/pb"
)

func main() {
	port := getenv("PORT", "8083")
	uri := getenv("MONGODB_URI", "mongodb://root:example@mongo:27017/?authSource=admin")
	dbName := getenv("MONGODB_DB", "bookings_db")
	roomsAddr := getenv("ROOMS_GRPC_ADDR", "rooms-svc:9002")
	internalToken := getenv("INTERNAL_TOKEN", "internal-call-secret")

	ctx := context.Background()
	mc, _ := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	db := mc.Database(dbName)
	bookings := db.Collection("bookings")
	waitlist := db.Collection("waitlist")
	ensureIndexes(ctx, bookings, waitlist)

	// gRPC client to rooms-svc
	conn, err := grpc.Dial(roomsAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil { log.Fatal(err) }
	roomsClient := roomspb.NewRoomsServiceClient(conn)

	r := gin.Default()

	// Create booking
	r.POST("/bookings", func(c *gin.Context) {
		uid := c.GetHeader("X-User-Id")
		var in struct{ RoomID, Start, End string }
		if err := c.ShouldBindJSON(&in); err != nil || in.RoomID=="" || in.Start=="" || in.End=="" {
			c.JSON(http.StatusBadRequest, gin.H{"error":"invalid input"}); return
		}
		roid, err := primitive.ObjectIDFromHex(in.RoomID); if err != nil { c.JSON(400, gin.H{"error":"bad room id"}); return }

		// gRPC open check
		ok, err := roomsOpenCheckGRPC(c.Request.Context(), roomsClient, internalToken, in.RoomID, in.Start, in.End, 1)
		if err != nil { c.JSON(502, gin.H{"error": err.Error()}); return }
		if !ok { c.JSON(400, gin.H{"error":"room not open"}); return }

		// overlap check
		cnt, err := bookings.CountDocuments(ctx, bson.M{
			"room_id": roid, "status":"confirmed",
			"$nor": []bson.M{
				{"end_at": bson.M{"$lte": in.Start}},
				{"start_at": bson.M{"$gte": in.End}},
			},
		})
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		if cnt > 0 { c.JSON(400, gin.H{"error":"room already booked"}); return }

		res, err := bookings.InsertOne(ctx, bson.M{
			"room_id": roid, "user_id": uid, "start_at": in.Start, "end_at": in.End, "status":"confirmed",
			"created_at": time.Now().UTC(),
		})
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		id := res.InsertedID.(primitive.ObjectID).Hex()
		c.JSON(http.StatusCreated, gin.H{"booking_id": id})
	})

	// Cancel (auto-promote)
	r.DELETE("/bookings/:id", func(c *gin.Context) {
		uid := c.GetHeader("X-User-Id")
		id := c.Param("id")
		bid, err := primitive.ObjectIDFromHex(id); if err != nil { c.JSON(400, gin.H{"error":"bad id"}); return }
		var b struct{ RoomID primitive.ObjectID `bson:"room_id"`; UserID string `bson:"user_id"`; Start string `bson:"start_at"`; End string `bson:"end_at"`; Status string `bson:"status"` }
		if err := bookings.FindOne(ctx, bson.M{"_id": bid}).Decode(&b); err != nil { c.JSON(404, gin.H{"error":"not found"}); return }
		if b.UserID != uid || b.Status != "confirmed" { c.JSON(403, gin.H{"error":"forbidden"}); return }
		if _, err := bookings.UpdateOne(ctx, bson.M{"_id": bid, "status":"confirmed"}, bson.M{"$set": bson.M{"status":"cancelled"}}); err != nil {
			c.JSON(500, gin.H{"error": err.Error()}); return
		}
		// pop earliest waiter
		var w struct{ UserID string `bson:"user_id"`; ID primitive.ObjectID `bson:"_id"` }
		err = waitlist.FindOne(ctx, bson.M{"room_id": b.RoomID, "start_at": b.Start, "end_at": b.End}, options.FindOne().SetSort(bson.D{{Key:"created_at", Value:1}})).Decode(&w)
		if err == nil {
			_, _ = waitlist.DeleteOne(ctx, bson.M{"_id": w.ID})
			// guard overlap
			cnt, _ := bookings.CountDocuments(ctx, bson.M{
				"room_id": b.RoomID, "status":"confirmed",
				"$nor": []bson.M{{"end_at": bson.M{"$lte": b.Start}}, {"start_at": bson.M{"$gte": b.End}}},
			})
			if cnt == 0 {
				_, _ = bookings.InsertOne(ctx, bson.M{
					"room_id": b.RoomID, "user_id": w.UserID, "start_at": b.Start, "end_at": b.End, "status":"confirmed",
					"created_at": time.Now().UTC(),
				})
			}
		}
		c.JSON(http.StatusOK, gin.H{"status":"cancelled"})
	})

	// Search using gRPC + overlap filter
	r.GET("/search", func(c *gin.Context) {
		start := c.Query("start"); end := c.Query("end"); minCap := atoi(c.Query("min_capacity"))
		// gRPC list rooms
		rooms, err := listRoomsGRPC(c.Request.Context(), roomsClient)
		if err != nil { c.JSON(502, gin.H{"error": err.Error()}); return }
		var out []gin.H
		for _, rm := range rooms {
			if rm.Capacity < minCap { continue }
			ok, err := roomsOpenCheckGRPC(c.Request.Context(), roomsClient, internalToken, rm.ID, start, end, minCap)
			if err != nil || !ok { continue }
			roid, _ := primitive.ObjectIDFromHex(rm.ID)
			cnt, err := bookings.CountDocuments(ctx, bson.M{
				"room_id": roid, "status":"confirmed",
				"$nor": []bson.M{
					{"end_at": bson.M{"$lte": start}},
					{"start_at": bson.M{"$gte": end}},
				},
			})
			if err == nil && cnt == 0 {
				out = append(out, gin.H{"id": rm.ID, "name": rm.Name, "capacity": rm.Capacity})
			}
		}
		c.JSON(http.StatusOK, out)
	})

	log.Println("bookings-svc :" + port)
	_ = r.Run(":" + port)
}

// ---- gRPC helpers ----

type RoomLite struct{ ID, Name string; Capacity int }

func roomsOpenCheckGRPC(ctx context.Context, cli roomspb.RoomsServiceClient, token, id, start, end string, minCap int) (bool, error) {
	resp, err := cli.OpenCheck(ctx, &roomspb.OpenCheckRequest{
		RoomId: id, Start: start, End: end, MinCapacity: int32(minCap), InternalToken: token,
	})
	if err != nil { return false, err }
	return resp.GetOk(), nil
}

func listRoomsGRPC(ctx context.Context, cli roomspb.RoomsServiceClient) ([]RoomLite, error) {
	resp, err := cli.ListRooms(ctx, &roomspb.ListRoomsRequest{})
	if err != nil { return nil, err }
	out := make([]RoomLite, 0, len(resp.GetRooms()))
	for _, r := range resp.GetRooms() {
		out = append(out, RoomLite{ID: r.GetId(), Name: r.GetName(), Capacity: int(r.GetCapacity())})
	}
	return out, nil
}

// ---- misc ----

func ensureIndexes(ctx context.Context, bookings, waitlist *mongo.Collection) {
	_, _ = bookings.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key:"room_id",Value:1},{Key:"status",Value:1},{Key:"start_at",Value:1},{Key:"end_at",Value:1}}},
	})
	_, _ = waitlist.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key:"room_id",Value:1},{Key:"start_at",Value:1},{Key:"end_at",Value:1},{Key:"created_at",Value:1}},
	})
}
func atoi(s string) int { var n int; for _,ch := range s { if ch<'0'||ch>'9' { return n }; n=n*10+int(ch-'0') }; return n }
func getenv(k, def string) string { if v:=os.Getenv(k); v!="" { return v }; return def }
