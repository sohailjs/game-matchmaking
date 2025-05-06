package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"log"
	"net/http"
	"sync"
	"time"
)

var ctx = context.Background()

const (
	MatchmakingPool  = "matchmaking_pool"
	PlayerJoin       = "player_join"
	WaitTimerExpired = "wait_timer_expired"
	SessionStart     = "session_start"
	leaderKey        = "leader-lock"
	lockExpiration   = 10 * time.Second // Lock expiration time
	heartbeatPeriod  = 5 * time.Second  // Heartbeat interval
)

type Matchmaking struct {
	Id             uuid.UUID
	playerIdToConn map[string]*websocket.Conn
	Sessions       []Session
	//playerIdToMMPlayer      map[string]MMPlayer
	mu                      sync.RWMutex
	disconnectedPlayersChan chan string
}

func (mm *Matchmaking) addPlayer(player MMPlayer, conn *websocket.Conn) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	//mm.playerIdToMMPlayer[player.UserId] = player
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
	ID      uuid.UUID       `json:"id"`
	Mode    string          `json:"mode"`
	Players map[string]bool `json:"players"`
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
var mm *Matchmaking

func init() {
	rdb = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "",
		DB:       0,
	})
	mm = &Matchmaking{
		Id:             uuid.New(),
		playerIdToConn: map[string]*websocket.Conn{},
		Sessions:       make([]Session, 0),
		//playerIdToMMPlayer:      map[string]MMPlayer{},
		disconnectedPlayersChan: make(chan string),
	}
}

const PlayerQueue = "br:queue"
const DisconnectedPlayersList = "disconnectedPlayers"
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
	if err != nil {
		log.Println("Error marshalling request:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"err": "internal server error"})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println("Error upgrading connection:", err)
		return
	}

	mm.addPlayer(MMPlayer{
		UserId: userId,
		Mode:   mode,
	}, conn)

	defer func() {
		conn.Close()
		mm.disconnectedPlayersChan <- userId
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
	go leaderCheckLoop()

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			fmt.Printf("total connections: %d\n", len(mm.playerIdToConn))
			if isLeader(mm.Id.String()) {
				mmTick()
			}
		}
	}()

	go listenToBroadCastChannel()
	go processDisconnectedPlayers()
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
	playerQueue, err := rdb.LRange(ctx, PlayerQueue, 0, -1).Result()
	if err != nil {
		fmt.Println("Error getting list values:", err)
		return
	}
	for _, player := range playerQueue {
		// check if player is disconnected before adding to session
		if _, exists := mm.playerIdToConn[player]; !exists {
			continue
		}
		sessionFound := false
		for i := range mm.Sessions {
			session := &mm.Sessions[i]
			if len(session.Players) < MaxPlayers {
				session.Players[player] = true
				sessionFound = true
				break
			}
		}
		if !sessionFound {
			//create new session
			mm.addSession(Session{
				ID:      uuid.New(),
				Mode:    "br",
				Players: map[string]bool{player: true},
			})
		}
	}
	//empty the queue
	_, err = rdb.LTrim(ctx, PlayerQueue, 1, 0).Result()
	if err != nil {
		log.Printf("error emptying queue: %s", err)
	}
}

func removeDisconnectedPlayersFromSessions() {
	sessionsIdsToBeDeleted := make([]string, 0)
	disconnectedPlayers, _ := rdb.LRange(ctx, DisconnectedPlayersList, 0, -1).Result()
	for _, userId := range disconnectedPlayers {
		for i := range mm.Sessions {
			session := &mm.Sessions[i]
			if _, exists := session.Players[userId]; exists {
				delete(session.Players, userId)
			}
			//if 0 players in session then remove session as well
			if len(session.Players) == 0 {
				sessionsIdsToBeDeleted = append(sessionsIdsToBeDeleted, session.ID.String())
			}
		}
	}

	//clear the disconnected queue once processed
	rdb.Del(ctx, DisconnectedPlayersList)

	for _, sessionId := range sessionsIdsToBeDeleted {
		for i := len(mm.Sessions) - 1; i >= 0; i-- {
			if mm.Sessions[i].ID.String() == sessionId {
				mm.Sessions = append(mm.Sessions[:i], mm.Sessions[i+1:]...)
				break
			}
		}
	}
}

func broadcastSessionStatusToPlayers() {
	for _, session := range mm.Sessions {
		for userId, _ := range session.Players {
			mm.mu.RLock()
			if _, exists := mm.playerIdToConn[userId]; exists {
				conn := mm.playerIdToConn[userId]
				conn.WriteJSON(session)
			}
			mm.mu.RUnlock()
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

func processDisconnectedPlayers() {
	for userId := range mm.disconnectedPlayersChan {
		mm.mu.Lock()
		delete(mm.playerIdToConn, userId)
		//delete(mm.playerIdToMMPlayer, userId)
		mm.mu.Unlock()
		rdb.LPush(ctx, DisconnectedPlayersList, userId)
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
			for userId, _ := range payload.SessionStatus.Players {
				mm.mu.RLock()
				if _, exists := mm.playerIdToConn[userId]; exists {
					conn := mm.playerIdToConn[userId]
					conn.WriteJSON(payload.SessionStatus)
				}
				mm.mu.RUnlock()
			}
		}
	}
}

func tryToBecomeLeader(instanceID string) bool {
	success, err := rdb.SetNX(ctx, leaderKey, instanceID, lockExpiration).Result()
	if err != nil {
		fmt.Println("Error trying to acquire leader lock:", err)
		return false
	}
	return success
}

func isLeader(instanceID string) bool {
	currentLeader, err := rdb.Get(ctx, leaderKey).Result()
	if err == redis.Nil {
		return false // No leader exists
	} else if err != nil {
		fmt.Println("Error checking leader status:", err)
		return false
	}
	return currentLeader == instanceID
}

func renewLeaderLock(instanceID string) {
	val, err := rdb.Get(ctx, leaderKey).Result()
	if err == nil && val == instanceID {
		rdb.Expire(ctx, leaderKey, lockExpiration)
	}
}

func leaderCheckLoop() {
	ticker := time.NewTicker(heartbeatPeriod)
	defer ticker.Stop()

	for range ticker.C {
		if tryToBecomeLeader(mm.Id.String()) {
			fmt.Println("I am the leader!")
			// Perform leader-specific tasks here
		} else if isLeader(mm.Id.String()) {
			//fmt.Println("Still the leader, renewing lock...")
			renewLeaderLock(mm.Id.String())
		} else {
			//fmt.Println("Not the leader, waiting for the next attempt...")
		}
	}
}

func mmTick() {
	removeDisconnectedPlayersFromSessions()
	//create sessions
	createSessions()
	//broadcast session status to players
	broadcastSessionStatusToPlayers()
	//broadcast session status to other MM servers
	broadcastSessionStatusToPubsub()

}
