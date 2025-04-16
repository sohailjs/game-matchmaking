package main

import (
	"github.com/google/uuid"
	"log"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	BaseURL        = "ws://localhost:8080/connect?userId="
	numConnections = 400               // Number of WebSocket connections to simulate
	duration       = 300 * time.Second // Duration to keep the connections open
)

func main() {
	var wg sync.WaitGroup
	for i := 0; i < numConnections; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			userId := generateUserID()
			//wsURL := BaseURL + userID + "&mode=br"
			u := url.URL{Scheme: "ws", Host: "localhost:8080", Path: "/connect"}
			query := url.Values{}
			query.Add("userId", userId)
			query.Add("mode", "br")
			u.RawQuery = query.Encode()
			wsConnect(u.String(), userId)
		}()
		time.Sleep(5 * time.Millisecond)
	}

	wg.Wait()
}

func wsConnect(url string, userId string) {
	log.Printf("connecting to %s", url)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		log.Printf("Error making socket connection: %v", err)
		return
	}
	log.Printf("Player Connected successfuly: %s", userId)
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
	return uuid.New().String()
}
