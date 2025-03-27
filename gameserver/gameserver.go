package main

import (
	"context"
	"fmt"
	"github.com/redis/go-redis/v9"
)

type GameServer struct {
	RoomCode string `json:"roomCode"`
	Mode     string `json:"mode"`
}

func NewGameServer(code string) *GameServer {
	return &GameServer{
		RoomCode: code,
	}
}

func (gs *GameServer) start() {
	fmt.Printf("Starting game server with room code: %s\n", gs.RoomCode)
	ctx := context.Background()
	rdb.HSet(ctx, "gameservers", "mode", gs.Mode)
	//create redis pubsub channel that MM will publish msg on
	pubsub := rdb.Subscribe(ctx, gs.RoomCode)
	go listenToPubsubChannel(pubsub)
}

func listenToPubsubChannel(pubsub *redis.PubSub) {
	ch := pubsub.Channel()
	for msg := range ch {
		fmt.Printf("received msg from MM: %v", msg)
	}
}

var rdb *redis.Client

func init() {
	rdb = redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
		DB:   0,
	})
}

func main() {
	gs := NewGameServer("1111")
	gs.start()
	select {}
}
