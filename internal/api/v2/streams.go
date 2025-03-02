// internal/api/v2/streams.go
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

// Constants for WebSocket connections
const (
	// Time allowed to write a message to the client
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the client
	pongWait = 60 * time.Second

	// Send pings to client with this period (must be less than pongWait)
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from client
	maxMessageSize = 512
)

var (
	// Upgrader for converting HTTP connections to WebSocket connections
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		// TODO: In production, this should be restricted to only allow specific origins
		// For example: CheckOrigin: func(r *http.Request) bool {
		//   origin := r.Header.Get("Origin")
		//   return isAllowedOrigin(origin)
		// }
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

// Client represents a connected WebSocket client
type Client struct {
	conn       *websocket.Conn
	send       chan []byte
	clientID   string
	streamType string
	lastSeen   time.Time
	closed     bool
	mu         sync.Mutex
	logger     *log.Logger
}

// logf is a helper for Client to log messages with the standard logger
func (client *Client) logf(format string, args ...interface{}) {
	if client.logger != nil {
		client.logger.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// initStreamRoutes registers all stream-related API endpoints
func (c *Controller) initStreamRoutes() {
	// Create streams API group with auth middleware
	streamsGroup := c.Group.Group("/streams", c.AuthMiddleware)

	// Routes for real-time data streams
	streamsGroup.GET("/audio-level", c.HandleAudioLevelStream)
	streamsGroup.GET("/notifications", c.HandleNotificationsStream)
}

// HandleAudioLevelStream handles WebSocket connections for streaming audio level data
func (c *Controller) HandleAudioLevelStream(ctx echo.Context) error {
	// Upgrade HTTP connection to WebSocket
	conn, err := upgrader.Upgrade(ctx.Response(), ctx.Request(), nil)
	if err != nil {
		c.LogfError("Error upgrading connection to WebSocket: %v", err)
		return err
	}

	// Create client
	client := &Client{
		conn:       conn,
		send:       make(chan []byte, 256),
		clientID:   ctx.Request().RemoteAddr,
		streamType: "audio-level",
		lastSeen:   time.Now(),
		logger:     log.Default(),
	}

	// Register client with global audio level clients map
	// This would typically be managed by a stream manager
	c.registerClient(client)

	// Start goroutines for reading and writing
	go client.writePump()
	go client.readPump(log.Default())

	return nil
}

// HandleNotificationsStream handles WebSocket connections for streaming notifications
func (c *Controller) HandleNotificationsStream(ctx echo.Context) error {
	// Upgrade HTTP connection to WebSocket
	conn, err := upgrader.Upgrade(ctx.Response(), ctx.Request(), nil)
	if err != nil {
		c.LogfError("Error upgrading connection to WebSocket: %v", err)
		return err
	}

	// Create client
	client := &Client{
		conn:       conn,
		send:       make(chan []byte, 256),
		clientID:   ctx.Request().RemoteAddr,
		streamType: "notifications",
		lastSeen:   time.Now(),
		logger:     log.Default(),
	}

	// Register client with global notifications clients map
	c.registerClient(client)

	// Start goroutines for reading and writing
	go client.writePump()
	go client.readPump(log.Default())

	return nil
}

// registerClient registers a WebSocket client with the appropriate stream manager
func (c *Controller) registerClient(client *Client) {
	// TODO: Implement proper client registration with the stream manager
	// TODO: Add client to a map of active clients with proper synchronization
	// TODO: Set up necessary event handling for broadcasting messages
	c.Debug("Client %s connected to %s stream", client.clientID, client.streamType)

	// This is where you would register with a stream manager that would
	// broadcast messages to all clients of a specific stream type
}

// unregisterClient removes a WebSocket client from the stream manager
func (c *Controller) unregisterClient(client *Client) {
	// In a real implementation, this would remove the client from the map of active clients
	c.Debug("Client %s disconnected from %s stream", client.clientID, client.streamType)
}

// writePump pumps messages from the application to the WebSocket connection
func (client *Client) writePump() {
	// Ensure logger is available or use a default one
	if client.logger == nil {
		client.logger = log.New(log.Writer(), "websocket: ", log.LstdFlags)
	}

	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		client.conn.Close()
	}()

	for {
		select {
		case message, ok := <-client.send:
			if err := client.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				client.logf("Failed to set write deadline: %v", err)
				return
			}

			if !ok {
				// The hub closed the channel
				if err := client.conn.WriteMessage(websocket.CloseMessage, []byte{}); err != nil {
					client.logf("Error writing close message: %v", err)
				}
				return
			}

			w, err := client.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				client.logf("Error getting writer: %v", err)
				return
			}

			if _, err := w.Write(message); err != nil {
				client.logf("Error writing message: %v", err)
				return
			}

			// Add queued messages to the current WebSocket message
			n := len(client.send)
			for i := 0; i < n; i++ {
				if _, err := w.Write([]byte{'\n'}); err != nil {
					client.logf("Error writing delimiter: %v", err)
					return
				}

				chunk := <-client.send
				if _, err := w.Write(chunk); err != nil {
					client.logf("Error writing chunk: %v", err)
					return
				}
			}

			if err := w.Close(); err != nil {
				client.logf("Error closing writer: %v", err)
				return
			}
		case <-ticker.C:
			if err := client.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				client.logf("Failed to set write deadline for ping: %v", err)
				return
			}

			if err := client.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				client.logf("Error writing ping message: %v", err)
				return
			}
		}
	}
}

// readPump pumps messages from the WebSocket connection to the hub
func (client *Client) readPump(logger *log.Logger) {
	// Store the logger in the client for consistency
	client.logger = logger

	defer func() {
		client.mu.Lock()
		client.closed = true
		client.mu.Unlock()
		client.conn.Close()
	}()

	client.conn.SetReadLimit(maxMessageSize)
	if err := client.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		client.logf("Failed to set initial read deadline: %v", err)
		return
	}

	client.conn.SetPongHandler(func(string) error {
		client.mu.Lock()
		client.lastSeen = time.Now()
		client.mu.Unlock()
		if err := client.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
			client.logf("Failed to set read deadline: %v", err)
			return err
		}
		return nil
	})

	for {
		_, message, err := client.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				logger.Printf("WebSocket error: %v", err)
			}
			break
		}

		// Process incoming message if needed
		// For most stream cases, clients are read-only and don't send messages
		// This could handle client subscription requests or filter updates
		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err == nil {
			// Handle message based on its content
			logger.Printf("Received message from client: %v", msg)
		}
	}
}
