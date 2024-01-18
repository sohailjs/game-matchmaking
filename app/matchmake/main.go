package main

import (
	"context"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"
)

var ctx = context.Background()

const (
	MatchmakingPool = "matchmaking_pool"
)

func main() {
	redisClient := redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "",
		DB:       0,
	})

	//ticker := time.NewTicker(1 * time.Second)

	go func() {
		for {
			//<-ticker.C
			res := findMatch(redisClient)
			if len(res) > 0 {
				fmt.Println("Match found:", res)
			}
		}
	}()
	select {}
}

func findMatch(redisClient *redis.Client) []string {
	var matchingPlayers []string
	res, err := redisClient.LRange(ctx, MatchmakingPool, -2, -1).Result()
	if err != nil {
		log.Fatal(err)
	}
	if len(res) == 2 {
		for i := 0; i < 2; i++ {
			result, err := redisClient.RPop(ctx, MatchmakingPool).Result()
			if err != nil {
				log.Fatal(err)
			}
			fmt.Printf("Removed player id: %s\n", result)
			matchingPlayers = append(matchingPlayers, result)
		}
	}
	return matchingPlayers
}
