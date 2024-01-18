package main

import (
	"context"

	"github.com/redis/go-redis/v9"
)

var ctx = context.Background()

func main() {
	redisClient := redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "",
		DB:       0,
	})
}
