package main

import (
	"context"
	"encoding/json"
	"flag"
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
	MatchmakingPool  = "matchmaking_pool"
	PlayerJoin       = "player_join"
	WaitTimerExpired = "wait_timer_expired"
	SessionStart     = "session_start"
)

type Matchmaking struct {
	Id                 uuid.UUID
	playerIdToConn     map[string]*websocket.Conn
	Sessions           []Session
	playerIdToMMPlayer map[string]MMPlayer
}

func (mm *Matchmaking) addPlayer(player MMPlayer, conn *websocket.Conn) {
	mm.playerIdToMMPlayer[player.UserId] = player
	mm.playerIdToConn[player.UserId] = conn
}

func (mm *Matchmaking) addSession(session Session) {
	mm.Sessions = append(mm.Sessions, session)
}

type MMPlayer struct {
	UserId string `json:"userId"`
	Mode   string `json:"mode"`
}

type Session struct {
	ID      uuid.UUID `json:"id"`
	Mode    string    `json:"mode"`
	Players []string  `json:"players"`
}

type BroadCastPayload struct {
	From          string  `json:"from	"`
	SessionStatus Session `json:"sessionStatus"`
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
var mutex *redsync.Mutex
var mm *Matchmaking

func init() {
	rdb = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "",
		DB:       0,
	})
	pool := goredis.NewPool(rdb)
	rs = redsync.New(pool)
	mutex = rs.NewMutex("lock")
	mm = &Matchmaking{
		Id:                 uuid.New(),
		playerIdToConn:     map[string]*websocket.Conn{},
		Sessions:           make([]Session, 0),
		playerIdToMMPlayer: map[string]MMPlayer{},
	}
}

const PlayerQueue = "br:queue"
const SessionStatusPubSub = "sessionStatus"
const EventQueue = "event"

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
	_, err := json.Marshal(newRequest)

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)

	mm.addPlayer(MMPlayer{
		UserId: userId,
		Mode:   mode,
	}, conn)

	defer func() {
		conn.Close()
		rdb.LRem(ctx, PlayerQueue, 1, userId)
	}()

	if err != nil {
		log.Println(err)
		return
	}
	_, err = rdb.LPush(ctx, PlayerQueue, userId).Result()
	if err != nil {
		log.Printf("Error pushing user %s to queue: %s", userId, err)
		conn.Close()
		return
	}
	log.Printf("User %s added to queue", userId)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		fmt.Println(msg)
	}
}

func main() {
	port := flag.String("port", "8080", "Port to run server on")
	flag.Parse()
	addr := fmt.Sprintf(":%s", *port)

	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		log.Fatal("can't connect to redis: ", err)
	}

	r := gin.Default()

	r.GET("/connect", handleWebSocket)

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			mutex.Lock()
			mmTick()
			mutex.Unlock()
		}
	}()

	go listenToBroadCastChannel()
	err = r.Run(addr)
	if err != nil {
		log.Fatal(err)
	}
}

const (
	MinPlayers = 2
	MaxPlayers = 2
	GameServer = "GameServer"
)

func createSessions() {
	listLength, err := rdb.LLen(ctx, PlayerQueue).Result()
	if err != nil {
		fmt.Println("Error getting list length:", err)
		return
	}
	playerQueue, err := rdb.LRange(ctx, PlayerQueue, 0, listLength-1).Result()
	if err != nil {
		fmt.Println("Error getting list values:", err)
		return
	}
	for _, player := range playerQueue {
		sessionFound := false
		for i := range mm.Sessions {
			session := &mm.Sessions[i]
			if len(session.Players) < MaxPlayers {
				session.Players = append(session.Players, player)
				sessionFound = true
				break
			}
		}
		if !sessionFound {
			//create new session
			mm.addSession(Session{
				ID:      uuid.New(),
				Mode:    "br",
				Players: []string{player},
			})
		}
	}
	//empty the queue
	_, err = rdb.LTrim(ctx, PlayerQueue, 1, 0).Result()
	if err != nil {
		log.Printf("error emptying queue: %s", err)
	}
}

func broadcastSessionStatusToPlayers() {
	for _, session := range mm.Sessions {
		for _, player := range session.Players {
			if _, exists := mm.playerIdToConn[player]; exists {
				conn := mm.playerIdToConn[player]
				conn.WriteJSON(session)
			}
		}
	}
}

func broadcastSessionStatusToPubsub() {
	for _, session := range mm.Sessions {
		payload := BroadCastPayload{
			From:          mm.Id.String(),
			SessionStatus: session,
		}
		broadcastPayload, _ := json.Marshal(payload)
		_, err := rdb.Publish(ctx, SessionStatusPubSub, broadcastPayload).Result()
		if err != nil {
			log.Printf("error publishing to pubsub: %s", err)
		}
	}
}

func listenToBroadCastChannel() {
	pubsub := rdb.Subscribe(ctx, SessionStatusPubSub)
	for {
		msg, ok := <-pubsub.Channel()
		if !ok {
			log.Printf("error getting data from channel")
			continue
		}
		var payload BroadCastPayload
		err := json.Unmarshal([]byte(msg.Payload), &payload)
		if err != nil {
			log.Printf("error unmarshalling json: %s", err)
			return
		}

		//broadcast session status received from pubsub channel to players
		if payload.From != mm.Id.String() {
			fmt.Printf("\nreceived from pubsub: %v", payload)
			for _, player := range payload.SessionStatus.Players {
				if _, exists := mm.playerIdToConn[player]; exists {
					conn := mm.playerIdToConn[player]
					conn.WriteJSON(payload.SessionStatus)
				}
			}
		}
	}
}
func mmTick() {
	//create sessions
	createSessions()
	//broadcast session status to players
	broadcastSessionStatusToPlayers()
	//broadcast session status to other MM servers
	broadcastSessionStatusToPubsub()
	/*// Get the length of the list
	listLength, err := rdb.LLen(ctx, PlayerQueue).Result()
	if err != nil {
		fmt.Println("Error getting list length:", err)
		return
	}

	// Retrieve the list values
	playerQueue, err := rdb.LRange(ctx, PlayerQueue, 0, listLength-1).Result()
	if err != nil {
		fmt.Println("Error getting list values:", err)
		return
	}

	servers, err := rdb.LRange(ctx, GameServer, 0, -1).Result()
	if err != nil {
		fmt.Println("Error getting servers:", err)
		return
	}

	var sessionData []Session
	var playersToRemove []string
	var serversToRemove []string
	for _, player := range playerQueue {

		//TODO: check if any existing game server accepting the players
		//
		//find session
		var sessionFound bool
		var sessionsReadyForAllocation []Session
		for i, _ := range sessionData {
			s := &sessionData[i]
			if len(s.Players) < MinPlayers {
				s.Players = append(s.Players, player)
				sessionFound = true
				if len(s.Players) == MinPlayers {
					sessionsReadyForAllocation = append(sessionsReadyForAllocation, *s)
				}
			}
		}
		if !sessionFound {
			sessionData = append(sessionData, Session{
				ID:      uuid.New(),
				Mode:    "br",
				Players: []string{player},
			})
		}

		//for all ready sessions for allocation, find game server

		i := 0
		for _, s := range sessionsReadyForAllocation {
			if len(servers) == 0 || i >= len(servers) {
				fmt.Println("No servers available")
				break
			}
			fmt.Printf("assigning session %s to server %s, players: %v\n", s.ID.String(), servers[i], s.Players)
			//remove players in session from redis queue
			playersToRemove = append(playersToRemove, s.Players...)
			//remove allocated server
			serversToRemove = append(serversToRemove, servers[i])
			i++
		}
	}
	//remove servers from redis
	for _, server := range serversToRemove {
		rdb.LRem(ctx, GameServer, 1, server)
	}
	// Remove players from Redis queue
	for _, playerId := range playersToRemove {
		rdb.LRem(ctx, PlayerQueue, 1, playerId)
	}*/
}
