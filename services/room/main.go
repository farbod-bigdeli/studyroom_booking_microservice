package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc"
	roomspb "rooms-svc/internal/pb"
)

type server struct {
	roomspb.UnimplementedRoomsServiceServer
	rooms *mongo.Collection
	sched *mongo.Collection
	token string
}

func main() {
	port := getenv("PORT", "8082")       // HTTP (admin/public)
	gport := getenv("GRPC_PORT", "9002") // gRPC (internal)
	uri := getenv("MONGODB_URI", "mongodb://root:example@mongo:27017/?authSource=admin")
	dbName := getenv("MONGODB_DB", "rooms_db")
	token := getenv("INTERNAL_TOKEN", "internal-call-secret")

	ctx := context.Background()
	mc, _ := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	db := mc.Database(dbName)
	rooms := db.Collection("rooms")
	sched := db.Collection("room_schedules")
	ensureIndexes(ctx, rooms, sched)

	// HTTP server (unchanged admin and listing)
	go func() {
		r := gin.Default()
		r.POST("/rooms", func(c *gin.Context) {
			var in struct{ Name string; Capacity int }
			if err := c.ShouldBindJSON(&in); err != nil || in.Name == "" || in.Capacity <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error":"invalid input"}); return
			}
			res, err := rooms.InsertOne(ctx, bson.M{"name": in.Name, "capacity": in.Capacity})
			if err != nil { c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()}); return }
			c.JSON(http.StatusCreated, gin.H{"id": res.InsertedID.(primitive.ObjectID).Hex()})
		})
		r.GET("/rooms", func(c *gin.Context) {
			cur, err := rooms.Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"name":1,"capacity":1}))
			if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
			defer cur.Close(ctx)
			var out []gin.H
			for cur.Next(ctx) {
				var doc struct{ ID primitive.ObjectID `bson:"_id"`; Name string `bson:"name"`; Capacity int `bson:"capacity"`}
				if err := cur.Decode(&doc); err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
				out = append(out, gin.H{"id": doc.ID.Hex(), "name": doc.Name, "capacity": doc.Capacity})
			}
			c.JSON(http.StatusOK, out)
		})
		r.POST("/rooms/:id/schedules", func(c *gin.Context) {
			roomID := c.Param("id")
			oid, err := primitive.ObjectIDFromHex(roomID); if err != nil { c.JSON(400, gin.H{"error":"bad room id"}); return }
			var in struct{ Start, End string; IsOpen bool }
			if err := c.ShouldBindJSON(&in); err != nil || in.Start=="" || in.End=="" {
				c.JSON(400, gin.H{"error":"invalid input"}); return
			}
			_, err = sched.InsertOne(ctx, bson.M{"room_id": oid, "start_at": in.Start, "end_at": in.End, "is_open": in.IsOpen})
			if err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
			c.JSON(http.StatusOK, gin.H{"status":"ok"})
		})
		log.Println("rooms-svc HTTP :" + port)
		_ = r.Run(":" + port)
	}()

	// gRPC server
	s := &server{rooms: rooms, sched: sched, token: token}
	gs := grpc.NewServer()
	roomspb.RegisterRoomsServiceServer(gs, s)
	lis, err := net.Listen("tcp", ":"+gport)
	if err != nil { log.Fatal(err) }
	log.Println("rooms-svc gRPC :" + gport)
	log.Fatal(gs.Serve(lis))
}

func (s *server) OpenCheck(ctx context.Context, req *roomspb.OpenCheckRequest) (*roomspb.OpenCheckResponse, error) {
	if req.GetInternalToken() != s.token { return &roomspb.OpenCheckResponse{Ok:false}, nil }
	oid, err := primitive.ObjectIDFromHex(req.GetRoomId()); if err != nil { return &roomspb.OpenCheckResponse{Ok:false}, nil }
	var rm struct{ Capacity int }
	if err := s.rooms.FindOne(ctx, bson.M{"_id": oid}).Decode(&rm); err != nil { return &roomspb.OpenCheckResponse{Ok:false}, nil }
	if int(req.GetMinCapacity()) > rm.Capacity { return &roomspb.OpenCheckResponse{Ok:false}, nil }
	cnt, err := s.sched.CountDocuments(ctx, bson.M{
		"room_id": oid, "is_open": true,
		"start_at": bson.M{"$lte": req.GetStart()},
		"end_at":   bson.M{"$gte": req.GetEnd()},
	})
	return &roomspb.OpenCheckResponse{Ok: err==nil && cnt>0}, nil
}

func (s *server) ListRooms(ctx context.Context, _ *roomspb.ListRoomsRequest) (*roomspb.ListRoomsResponse, error) {
	cur, err := s.rooms.Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"name":1,"capacity":1}))
	if err != nil { return nil, err }
	defer cur.Close(ctx)
	var rs []*roomspb.Room
	for cur.Next(ctx) {
		var doc struct{ ID primitive.ObjectID `bson:"_id"`; Name string `bson:"name"`; Capacity int `bson:"capacity"` }
		if err := cur.Decode(&doc); err != nil { return nil, err }
		rs = append(rs, &roomspb.Room{Id: doc.ID.Hex(), Name: doc.Name, Capacity: int32(doc.Capacity)})
	}
	return &roomspb.ListRoomsResponse{Rooms: rs}, cur.Err()
}

func ensureIndexes(ctx context.Context, rooms, sched *mongo.Collection) {
	_, _ = rooms.Indexes().CreateOne(ctx, mongo.IndexModel{ Keys: bson.D{{Key:"name",Value:1}}, Options: options.Index().SetUnique(true) })
	_, _ = sched.Indexes().CreateOne(ctx, mongo.IndexModel{ Keys: bson.D{{Key:"room_id",Value:1},{Key:"is_open",Value:1},{Key:"start_at",Value:1},{Key:"end_at",Value:1}} })
}
func getenv(k, def string) string { if v:=os.Getenv(k); v!="" { return v }; return def }
