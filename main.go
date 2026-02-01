package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Studio proxy
var studioURL, _ = url.Parse("http://localhost:3000")
var studioProxy = httputil.NewSingleHostReverseProxy(studioURL)

// StudioHandler wraps the proxy with proper path handling
func studioHandler(w http.ResponseWriter, r *http.Request) {
	// Strip /openclaw-studio prefix
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/openclaw-studio")
	if r.URL.Path == "" {
		r.URL.Path = "/"
	}
	studioProxy.ServeHTTP(w, r)
}

var db *pgxpool.Pool

// City state: "q,r" -> cell data
type CityCell struct {
	Q            int         `json:"q"`
	R            int         `json:"r"`
	Type         string      `json:"type"`
	UserID       string      `json:"userId"`
	Timestamp    int64       `json:"timestamp"`
	BuildingData interface{} `json:"buildingData,omitempty"`
}

var cityState = make(map[string]CityCell)
var cityMu sync.RWMutex

// WebSocket hub for multiplayer
type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
}

type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
	id   string
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WebSocket proxy to Gateway
func handleGatewayWS(w http.ResponseWriter, r *http.Request) {
	// Connect to local Gateway
	gatewayURL := "ws://127.0.0.1:18789"
	if os.Getenv("GATEWAY_URL") != "" {
		gatewayURL = os.Getenv("GATEWAY_URL")
	}
	
	// Open connection to Gateway
	gatewayConn, _, err := websocket.DefaultDialer.Dial(gatewayURL, nil)
	if err != nil {
		log.Printf("Failed to connect to Gateway: %v", err)
		http.Error(w, "Gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	defer gatewayConn.Close()
	
	// Upgrade client connection
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade client: %v", err)
		return
	}
	defer clientConn.Close()
	
	// Bidirectional proxy
	errChan := make(chan error, 2)
	
	// Client -> Gateway
	go func() {
		for {
			msgType, data, err := clientConn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
			if err := gatewayConn.WriteMessage(msgType, data); err != nil {
				errChan <- err
				return
			}
		}
	}()
	
	// Gateway -> Client
	go func() {
		for {
			msgType, data, err := gatewayConn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
			if err := clientConn.WriteMessage(msgType, data); err != nil {
				errChan <- err
				return
			}
		}
	}()
	
	<-errChan
}

var hub = &Hub{
	clients:    make(map[*Client]bool),
	broadcast:  make(chan []byte),
	register:   make(chan *Client),
	unregister: make(chan *Client),
}

var serverStartTime = time.Now().UnixMilli()

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			count := len(h.clients)
			h.mu.Unlock()
			
			// Send server time to new client for sync
			syncMsg, _ := json.Marshal(map[string]interface{}{
				"type":      "sync",
				"id":        client.id,
				"startTime": serverStartTime,
				"now":       time.Now().UnixMilli(),
			})
			client.send <- syncMsg
			
			// Send full city state
			cityMu.RLock()
			cityArray := make([]CityCell, 0, len(cityState))
			for _, cell := range cityState {
				cityArray = append(cityArray, cell)
			}
			cityMu.RUnlock()

			citySyncMsg, _ := json.Marshal(map[string]interface{}{
				"type": "city_sync",
				"city": cityArray,
			})
			client.send <- citySyncMsg

			// Send existing rectangles
			rects, err := loadRectangles()
			if err != nil {
				log.Printf("Failed to load rectangles: %v", err)
			} else {
				log.Printf("Sending %d rectangles to client", len(rects))
				rectSyncMsg, _ := json.Marshal(map[string]interface{}{
					"type":       "rect_sync",
					"rectangles": rects,
				})
				client.send <- rectSyncMsg
			}

			// Broadcast participant count
			h.broadcastCount(count)

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			count := len(h.clients)
			h.mu.Unlock()
			h.broadcastCount(count)

		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (h *Hub) broadcastCount(count int) {
	msg, _ := json.Marshal(map[string]interface{}{
		"type":  "count",
		"count": count,
	})
	h.mu.RLock()
	for client := range h.clients {
		select {
		case client.send <- msg:
		default:
		}
	}
	h.mu.RUnlock()
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		
		var data map[string]interface{}
		if json.Unmarshal(message, &data) != nil {
			continue
		}
		
		msgType, _ := data["type"].(string)
		
		switch msgType {
		case "city_update":
			// Store city update
			q, _ := data["q"].(float64)
			r, _ := data["r"].(float64)
			terrain, _ := data["terrain"].(string)
			buildingData, _ := data["buildingData"]
			
			key := fmt.Sprintf("%d,%d", int(q), int(r))
			cell := CityCell{
				Q:            int(q),
				R:            int(r),
				Type:         terrain,
				UserID:       c.id,
				Timestamp:    time.Now().UnixMilli(),
				BuildingData: buildingData,
			}
			
			cityMu.Lock()
			cityState[key] = cell
			cityMu.Unlock()
			
			// Broadcast to others
			data["id"] = c.id
			msg, _ := json.Marshal(data)
			c.hub.broadcast <- msg
			
		case "city_cursor":
			// Just broadcast cursor position
			data["id"] = c.id
			msg, _ := json.Marshal(data)
			c.hub.broadcast <- msg
			
		case "rect_create":
			// Save rectangle to database
			rect := Rectangle{
				RectID:      data["rectId"].(string),
				X:           data["x"].(float64),
				Y:           data["y"].(float64),
				Width:       data["width"].(float64),
				Height:      data["height"].(float64),
				BorderColor: data["borderColor"].(string),
				FillColor:   data["fillColor"].(string),
				UserID:      c.id,
			}
			if err := saveRectangle(rect); err != nil {
				log.Printf("Failed to save rectangle: %v", err)
			}
			// Broadcast to others
			data["id"] = c.id
			msg, _ := json.Marshal(data)
			c.hub.broadcast <- msg

		case "rect_delete":
			// Delete rectangle from database
			rectId, _ := data["rectId"].(string)
			if rectId != "" {
				if err := deleteRectangle(rectId); err != nil {
					log.Printf("Failed to delete rectangle: %v", err)
				}
				// Broadcast to others
				data["id"] = c.id
				msg, _ := json.Marshal(data)
				c.hub.broadcast <- msg
			}

		default:
			// Default: add client ID and broadcast
			data["id"] = c.id
			msg, _ := json.Marshal(data)
			c.hub.broadcast <- msg
		}
	}
}

func (c *Client) writePump() {
	defer c.conn.Close()
	for message := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
			break
		}
	}
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &Client{
		hub:  hub,
		conn: conn,
		send: make(chan []byte, 256),
		id:   generateID(),
	}
	hub.register <- client
	go client.writePump()
	go client.readPump()
}

func generateID() string {
	return time.Now().Format("150405.000")
}

func main() {
	// Database connection
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://appuser:apppass@localhost:5432/appdb"
	}

	var err error
	db, err = pgxpool.New(context.Background(), dbURL)
	if err != nil {
		log.Fatal("Unable to connect to database:", err)
	}
	defer db.Close()

	// Create tables
	_, err = db.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS items (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		)
	`)
	if err != nil {
		log.Fatal("Unable to create items table:", err)
	}

	_, err = db.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS rectangles (
			rect_id TEXT PRIMARY KEY,
			x DOUBLE PRECISION NOT NULL,
			y DOUBLE PRECISION NOT NULL,
			width DOUBLE PRECISION NOT NULL,
			height DOUBLE PRECISION NOT NULL,
			border_color TEXT NOT NULL,
			fill_color TEXT NOT NULL,
			user_id TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		)
	`)
	if err != nil {
		log.Fatal("Unable to create rectangles table:", err)
	}

	// Start WebSocket hub
	go hub.run()

	// Routes
	mux := http.NewServeMux()
	
	// Proxy Next.js static assets (direct path, no strip needed)
	mux.Handle("/_next/", studioProxy)
	
	// Gateway WebSocket proxy for studio (MUST be before /openclaw-studio/)
	mux.HandleFunc("/openclaw-studio/ws", handleGatewayWS)
	
	// Proxy /openclaw-studio to Next.js app (strip the prefix)
	mux.HandleFunc("/openclaw-studio/", studioHandler)
	mux.Handle("/openclaw-studio", http.RedirectHandler("/openclaw-studio/", http.StatusMovedPermanently))
	
	// API and specific routes
	mux.HandleFunc("GET /rectangle", handleRectangle)
	mux.HandleFunc("GET /city", handleCity)
	mux.HandleFunc("GET /favicon.svg", handleFavicon)
	mux.HandleFunc("GET /ws", handleWebSocket)
	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("GET /api/items", handleGetItems)
	mux.HandleFunc("POST /api/items", handleCreateItem)
	
	// Root handler - must be last
	mux.HandleFunc("/", handleHome)

	// Server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func handleHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/index.html")
}

func handleRectangle(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/rectangle.html")
}

func handleCity(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/city.html")
}

func handleFavicon(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/favicon.svg")
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	err := db.Ping(context.Background())
	if err != nil {
		http.Error(w, "database unhealthy", http.StatusServiceUnavailable)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

type Item struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type Rectangle struct {
	RectID      string  `json:"rectId"`
	X           float64 `json:"x"`
	Y           float64 `json:"y"`
	Width       float64 `json:"width"`
	Height      float64 `json:"height"`
	BorderColor string  `json:"borderColor"`
	FillColor   string  `json:"fillColor"`
	UserID      string  `json:"userId"`
}

func saveRectangle(rect Rectangle) error {
	_, err := db.Exec(context.Background(), `
		INSERT INTO rectangles (rect_id, x, y, width, height, border_color, fill_color, user_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (rect_id) DO UPDATE SET
			x = EXCLUDED.x,
			y = EXCLUDED.y,
			width = EXCLUDED.width,
			height = EXCLUDED.height,
			border_color = EXCLUDED.border_color,
			fill_color = EXCLUDED.fill_color
	`, rect.RectID, rect.X, rect.Y, rect.Width, rect.Height, rect.BorderColor, rect.FillColor, rect.UserID)
	return err
}

func loadRectangles() ([]Rectangle, error) {
	rows, err := db.Query(context.Background(), `
		SELECT rect_id, x, y, width, height, border_color, fill_color, user_id
		FROM rectangles ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rects []Rectangle
	for rows.Next() {
		var r Rectangle
		if err := rows.Scan(&r.RectID, &r.X, &r.Y, &r.Width, &r.Height, &r.BorderColor, &r.FillColor, &r.UserID); err != nil {
			continue
		}
		rects = append(rects, r)
	}
	return rects, nil
}

func deleteRectangle(rectID string) error {
	_, err := db.Exec(context.Background(), `DELETE FROM rectangles WHERE rect_id = $1`, rectID)
	return err
}

func handleGetItems(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(context.Background(), "SELECT id, name, created_at FROM items ORDER BY id")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var item Item
		rows.Scan(&item.ID, &item.Name, &item.CreatedAt)
		items = append(items, item)
	}

	if items == nil {
		items = []Item{}
	}
	json.NewEncoder(w).Encode(items)
}

func handleCreateItem(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	var item Item
	err := db.QueryRow(context.Background(),
		"INSERT INTO items (name) VALUES ($1) RETURNING id, name, created_at",
		input.Name,
	).Scan(&item.ID, &item.Name, &item.CreatedAt)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(item)
}
