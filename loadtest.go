package main

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	BaseURL        = "ws://localhost:8080/connect?userId="
	numConnections = 10                // Number of WebSocket connections to simulate
	duration       = 300 * time.Second // Duration to keep the connections open
)

func main() {
	var wg sync.WaitGroup
	for i := 0; i < numConnections; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			userID := generateUserID()
			wsURL := BaseURL + userID + "&mode=br"
			wsConnect(wsURL, userID)
		}()
		time.Sleep(2 * time.Millisecond)
	}

	wg.Wait()
}

func wsConnect(urlStr string, userId string) {
	u, err := url.Parse(urlStr)
	if err != nil {
		log.Printf("failed to parse URL: %v", err)
		return
	}

	log.Printf("connecting to %s", u.String())

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Printf("Error making socket connection: %v", err)
		return
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(duration))
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// Timeout error is expected
				log.Printf("read timeout: %v", err)
			} else {
				log.Printf("read error: %v", err)
			}
			conn.Close()
			return
		}
	}
}

func generateUserID() string {
	return fmt.Sprintf("%d", rand.Intn(1000000))
}
