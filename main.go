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
	"sync"
	"time"
)

var ctx = context.Background()

const (
	MatchmakingPool  = "matchmaking_pool"
	PlayerJoin       = "player_join"
	WaitTimerExpired = "wait_timer_expired"
	SessionStart     = "session_start"

	// Redsync constants
	LeaderLockKey   = "matchmaking:leader"
	LockTTL         = 30 * time.Second // Lock expiration time
	LockRetryDelay  = 1 * time.Second  // Retry delay for acquiring lock
	LockRetryTimes  = 3                // Number of retry attempts
	HeartbeatPeriod = 10 * time.Second // How often to renew the lock
)

type Matchmaking struct {
	Id                      uuid.UUID
	playerIdToConn          map[string]*websocket.Conn
	Sessions                map[string]*Session
	PlayerIdToSessionId     map[string]string
	mu                      sync.RWMutex
	disconnectedPlayersChan chan string

	// Redsync components
	redsync      *redsync.Redsync
	leaderLock   *redsync.Mutex
	isLeaderFlag bool
	leaderMu     sync.RWMutex
}

func (mm *Matchmaking) addPlayer(player MMPlayer, conn *websocket.Conn) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	mm.playerIdToConn[player.UserId] = conn
}

func (mm *Matchmaking) isLeader() bool {
	mm.leaderMu.RLock()
	defer mm.leaderMu.RUnlock()
	return mm.isLeaderFlag
}

func (mm *Matchmaking) setLeaderStatus(isLeader bool) {
	mm.leaderMu.Lock()
	defer mm.leaderMu.Unlock()
	mm.isLeaderFlag = isLeader
}

// Try to acquire leadership with Redsync
func (mm *Matchmaking) tryBecomeLeader() bool {
	// Create a new mutex for leader election
	mutex := mm.redsync.NewMutex(LeaderLockKey,
		redsync.WithExpiry(LockTTL),
		redsync.WithTries(LockRetryTimes),
		redsync.WithRetryDelay(LockRetryDelay),
		redsync.WithValue(mm.Id.String()), // Use instance ID as lock value
	)

	err := mutex.Lock()
	if err != nil {
		mm.setLeaderStatus(false)
		return false
	}

	// Successfully acquired the lock
	mm.leaderLock = mutex
	mm.setLeaderStatus(true)
	log.Printf("Node %s became the leader", mm.Id.String())
	return true
}

// Renew leadership lock
func (mm *Matchmaking) renewLeadership() bool {
	if mm.leaderLock == nil {
		return false
	}

	// Extend the lock
	ok, err := mm.leaderLock.Extend()
	if err != nil || !ok {
		log.Printf("Failed to renew leadership: %v", err)
		mm.releaseLeadership()
		return false
	}

	return true
}

// Release leadership
func (mm *Matchmaking) releaseLeadership() {
	if mm.leaderLock != nil {
		ok, err := mm.leaderLock.Unlock()
		if err != nil || !ok {
			log.Printf("Error releasing leadership lock: %v", err)
		}
		mm.leaderLock = nil
	}
	mm.setLeaderStatus(false)
	log.Printf("Node %s released leadership", mm.Id.String())
}

// Background leadership management
func (mm *Matchmaking) leadershipLoop() {
	ticker := time.NewTicker(HeartbeatPeriod)
	defer ticker.Stop()

	for range ticker.C {
		if mm.isLeader() {
			// Try to renew existing leadership
			if !mm.renewLeadership() {
				log.Println("Lost leadership, will try to reacquire")
			}
		} else {
			// Try to become leader
			mm.tryBecomeLeader()
		}
	}
}

type MMPlayer struct {
	UserId string `json:"userId"`
	Mode   string `json:"mode"`
}

type Session struct {
	ID      uuid.UUID       `json:"id"`
	Mode    string          `json:"mode"`
	Players map[string]bool `json:"players"`
	RoomId  string          `json:"roomId"` // Optional field for room ID
}

type BroadCastPayload struct {
	From          string  `json:"from"`
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

	// Initialize Redsync
	pool := goredis.NewPool(rdb)
	rs := redsync.New(pool)

	mm = &Matchmaking{
		Id:                      uuid.New(),
		playerIdToConn:          map[string]*websocket.Conn{},
		Sessions:                map[string]*Session{},
		PlayerIdToSessionId:     map[string]string{},
		disconnectedPlayersChan: make(chan string),
		redsync:                 rs,
		isLeaderFlag:            false,
	}
}

const (
	MinPlayers              = 2
	MaxPlayers              = 2
	GameServer              = "GameServer"
	PlayerQueue             = "br:queue"
	DisconnectedPlayersList = "disconnectedPlayers"
	SessionStatusPubSub     = "sessionStatus"
)

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

	conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	mm.addPlayer(MMPlayer{
		UserId: userId,
		Mode:   mode,
	}, conn)

	defer func() {
		mm.disconnectedPlayersChan <- userId
		conn.Close()
	}()

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

func createSessions() {
	playerQueue, err := rdb.LRange(ctx, PlayerQueue, 0, -1).Result()
	if err != nil {
		fmt.Println("Error getting list values:", err)
		return
	}

	processedPlayers := []string{}

	for _, player := range playerQueue {
		sessionFound := false
		for id := range mm.Sessions {
			session := mm.Sessions[id]
			if len(session.Players) < MaxPlayers {
				session.Players[player] = true
				sessionFound = true
				mm.PlayerIdToSessionId[player] = id
				processedPlayers = append(processedPlayers, player)
				break
			}
		}

		if !sessionFound {
			id := uuid.New()
			mm.Sessions[id.String()] = &Session{
				ID:      id,
				Mode:    "br",
				Players: map[string]bool{player: true},
			}
			mm.PlayerIdToSessionId[player] = id.String()
			processedPlayers = append(processedPlayers, player)
		}
	}
	// Only remove the players that were actually processed
	for _, player := range processedPlayers {
		rdb.LRem(ctx, PlayerQueue, 1, player)
	}
}

func removeDisconnectedPlayersFromSessions() {
	disconnectedPlayers, _ := rdb.LRange(ctx, DisconnectedPlayersList, 0, -1).Result()
	for _, userId := range disconnectedPlayers {
		//remove from queue if exists TODO: optimize this removing from queue
		rdb.LRem(ctx, PlayerQueue, 1, userId)

		sessionId, ok := mm.PlayerIdToSessionId[userId]
		if !ok {
			log.Printf("User %s not found in any session", userId)
			continue
		}
		session, exists := mm.Sessions[sessionId]
		if !exists {
			log.Printf("Session %s not found ", sessionId)
			continue
		}
		delete(session.Players, userId)
		delete(mm.PlayerIdToSessionId, userId)

		if len(session.Players) == 0 {
			delete(mm.Sessions, sessionId)
		}
	}
	// Clear the disconnected players list after processing
	rdb.Del(ctx, DisconnectedPlayersList)
}

func broadcastSessionStatusToPlayers() {
	for _, session := range mm.Sessions {
		for userId := range session.Players {
			mm.mu.RLock()
			if conn, exists := mm.playerIdToConn[userId]; exists {
				err := conn.WriteJSON(session)
				if err != nil {
					log.Printf("error sending session status to user %s: %s", userId, err)
				}
			}
			mm.mu.RUnlock()
		}
	}
}

func broadcastSessionStatusToPubsub() {
	for _, session := range mm.Sessions {
		payload := BroadCastPayload{
			From:          mm.Id.String(),
			SessionStatus: *session,
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
		mm.mu.Unlock()
		rdb.LPush(ctx, DisconnectedPlayersList, userId)
	}
}

func listenToBroadCastChannel() {
	pubsub := rdb.Subscribe(ctx, SessionStatusPubSub)
	defer pubsub.Close()

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
			continue
		}

		//broadcast session status received from pubsub channel to players
		if payload.From != mm.Id.String() {
			//log.Printf("\nreceived from pubsub: %v", payload)
			for userId := range payload.SessionStatus.Players {
				mm.mu.RLock()
				if conn, exists := mm.playerIdToConn[userId]; exists {
					conn.WriteJSON(payload.SessionStatus)
				}
				mm.mu.RUnlock()
			}
		}
	}
}

/*func assignServersToSessions() {
	//if session has min players required then assign them game server
	mm.mu.Lock()
	defer mm.mu.Unlock()
	for i := range mm.Sessions {
		if len(mm.Sessions[i].Players) >= MinPlayers {
			session := &mm.Sessions[i]
			// fetch game server from redis & assign it to this session
			server, err := rdb.LPop(ctx, GameServer).Result()
			if err != nil {
				log.Printf("Error fetching game server: %s", err)
				continue
			}
			if server == "" {
				log.Printf("No game server available for session %s", session.ID.String())
				continue
			}
			session.RoomId = server // This is a placeholder, replace with actual server assignment logic
			log.Printf("Assigned game server to session %s with players: %v", session.ID, session.Players)
		}
	}
}*/

func mmTick() {
	// Double-check leadership before processing (extra safety)
	if !mm.isLeader() {
		return
	}

	removeDisconnectedPlayersFromSessions()

	// Check leadership again after potentially long operation
	if !mm.isLeader() {
		log.Println("Lost leadership during tick, aborting remaining operations")
		return
	}

	createSessions()
	//assignServersToSessions()
	if !mm.isLeader() {
		log.Println("Lost leadership during tick, aborting remaining operations")
		return
	}

	broadcastSessionStatusToPlayers()
	broadcastSessionStatusToPubsub()
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

	// Add status endpoint
	/*r.GET("/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"nodeId":   mm.Id.String(),
			"isLeader": mm.isLeader(),
			"sessions": len(mm.Sessions),
			"players":  len(mm.playerIdToConn),
		})
	})*/

	// Start leadership management loop
	go mm.leadershipLoop()

	// Main game loop - only leader processes
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			mm.mu.RLock()
			log.Printf("\nNode %s - total connections: %d, total sessions: %d,isLeader: %t\n",
				mm.Id.String(), len(mm.playerIdToConn), len(mm.Sessions), mm.isLeader())
			mm.mu.RUnlock()

			if mm.isLeader() {
				t1 := time.Now()
				mmTick()
				t2 := time.Now()
				if t2.Sub(t1).Milliseconds() > 1000 {
					log.Printf("Node %s took too long to process tick: %d ms", mm.Id.String(), t2.Sub(t1).Milliseconds())
				}
			}
		}
	}()

	go listenToBroadCastChannel()
	go processDisconnectedPlayers()

	// Graceful shutdown
	defer func() {
		mm.releaseLeadership()
	}()

	log.Printf("Starting matchmaking server on %s", addr)
	err = r.Run(addr)
	if err != nil {
		log.Fatal(err)
	}
}
