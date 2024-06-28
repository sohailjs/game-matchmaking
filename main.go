package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v9"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"log"
	"net/http"
	"time"
)

var ctx = context.Background()

const (
	MatchmakingPool = "matchmaking_pool"
)

type session struct {
	ID      uuid.UUID `json:"id"`
	Mode    string    `json:"mode"`
	Players []string  `json:"players"`
}

type request struct {
	UserId string `json:"userId"`
	Mode   string `json:"mode"`
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var rdb *redis.Client
var rs *redsync.Redsync

func init() {
	rdb = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "",
		DB:       0,
	})
	pool := goredis.NewPool(rdb)
	rs = redsync.New(pool)
}

const PlayerQueue = "queue"

func handleWebSocket(c *gin.Context) {
	userId := c.Query("userId")
	mode := c.Query("mode")

	if userId == "" {
		log.Println("userId is empty")
		c.JSON(http.StatusBadRequest, gin.H{"err": "userId not provided"})
		return
	}
	newRequest := request{
		UserId: userId,
		Mode:   mode,
	}
	data, err := json.Marshal(newRequest)

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	defer func() {
		conn.Close()
		rdb.LRem(ctx, PlayerQueue, 1, data)
	}()

	if err != nil {
		log.Println(err)
		return
	}
	//accept different command from player. eg JOIN_GAME,LEAVE_GAME

	if err != nil {
		log.Println(err)
		return
	}
	rdb.LPush(ctx, PlayerQueue, data)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		fmt.Println(msg)
	}
}

func main() {

	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		log.Fatal("can't connect to redis: ", err)
	}

	r := gin.Default()

	r.GET("/connect", handleWebSocket)

	go func() {
		for {
			processRedisQueue()
		}
	}()

	go processTimers()

	err = r.Run(":8080")
	if err != nil {
		log.Fatal(err)
	}
}

func processRedisQueue() {
	// BLPOP blocks until an item is available in the queue
	result, err := rdb.BLPop(ctx, 0*time.Second, PlayerQueue).Result()
	if err != nil {
		fmt.Println("Error processing queue:", err)
		return
	}
	if len(result) > 1 {
		data := result[1] // The first element is the queue name, the second is the data
		processData(data)
	}
}

func processData(data string) {
	log.Println("Processing data:", data)
	var req request
	err := json.Unmarshal([]byte(data), &req)
	if err != nil {
		return
	}
	// find session, if not available then create one
	allSessions := getAllSessions()
	var availableSession *session
	for _, s := range allSessions {
		if s.Mode == req.Mode && len(s.Players) < 3 {
			availableSession = &s
			break
		}
	}
	if availableSession == nil {
		session := session{
			ID:      uuid.New(),
			Mode:    req.Mode,
			Players: []string{req.UserId},
		}
		jsonData, err := json.Marshal(session)

		_, err = rdb.Set(ctx, "session:"+session.ID.String(), jsonData, 0).Result()
		if err != nil {
			log.Println("Error creating session:", err)
		}
		log.Printf("Adding player to new session: %s", session.ID.String())
	} else {
		log.Printf("Adding player %s to existing session: %s", req.UserId, availableSession.ID.String())
		availableSession.Players = append(availableSession.Players, req.UserId)
		jsonData, err := json.Marshal(availableSession)
		if err != nil {
			log.Println("Error marshaling session data:", err)
			return
		}
		_, err = rdb.Set(ctx, "session:"+availableSession.ID.String(), jsonData, 0).Result()
		if err != nil {
			log.Println("Error updating session data:", err)
		}
		if len(availableSession.Players) == 2 {
			//start waiting_timer to start game when minimum players are available
			rdb.SetNX(ctx, "wait_timer:"+availableSession.ID.String(), "1", 20*time.Second)
		} else if len(availableSession.Players) >= 3 {
			//start the game as max players threshold reached
			log.Printf("max players reached, starting session: %s", availableSession.ID.String())
			rdb.Del(ctx, "wait_timer:"+availableSession.ID.String())
		}
	}
}

func processTimers() {
	pubsub := rdb.PSubscribe(ctx, "__keyevent@0__:expired")

	for msg := range pubsub.Channel() {
		mutex := rs.NewMutex("lock:timerProcess")
		if err := mutex.Lock(); err != nil {
			log.Println("lock not acquired:", err)
		} else {
			log.Println("lock acquired")
			log.Printf("wait timer expired, starting session: %s", msg.Payload)

			if ok, err := mutex.Unlock(); !ok || err != nil {
				log.Printf("could not release lock: %v", err)
			} else {
				log.Printf("lock released: %v", msg.Payload)
			}
		}
	}
}

func getAllSessions() []session {
	var sessions []session
	keys, err := rdb.Keys(ctx, "session:*").Result()
	if err != nil {
		log.Println("Error getting sessions:", err)
		return sessions
	}
	for _, key := range keys {
		data, err := rdb.Get(ctx, key).Result()
		if err != nil {
			log.Println("Error getting session data:", err)
			continue
		}
		var s session
		err = json.Unmarshal([]byte(data), &s)
		if err != nil {
			log.Println("Error unmarshalling session data:", err)
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions
}
