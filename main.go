package main

import (
	"context"
	"log"
	"strconv"

	"github.com/redis/go-redis/v9"
)

var ctx = context.Background()

func main() {

	redisClient := redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "",
		DB:       0,
	})

	for i := 0; i < 100; i++ {
		addPlayer(redisClient, "P"+strconv.Itoa(i))
	}

}

func addPlayer(redisClient *redis.Client, playerId string) {
	err := redisClient.LPush(ctx, "matchmaking_pool", playerId).Err()
	if err != nil {
		log.Fatal(err)
	}
}
