package main

import (
	"database/sql"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"
)

var addr = flag.String("addr", ":8080", "http service address")
var upgrader = websocket.Upgrader{}
var db *sql.DB

var (
	sharedPublicToken string
	sharedSecretToken string
)

var (
	clientsMu sync.RWMutex
	clients   = make(map[*websocket.Conn]chan []byte)
)

var (
	stmtInsertCode *sql.Stmt
)

func broadcastMessage(message []byte) {
	clientsMu.RLock()
	defer clientsMu.RUnlock()
	for _, ch := range clients {
		select {
		case ch <- message:
		default:
			slog.Warn("dropping message for slow client")
		}
	}
}

func websocketHandler(w http.ResponseWriter, req *http.Request) {
	if req.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		io.WriteString(w, "method not allowed")
		return
	} else if req.Header.Get("X-Public-Token") != sharedPublicToken {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "unauthorized")
		return
	}

	ws, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		slog.Error("error upgrading to websocket on /ws", "error", err.Error())
		return
	}
	defer ws.Close()

	// optimizing for sheer speed, not reliability
	if tcpConn, ok := ws.UnderlyingConn().(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
	}

	slog.Info("new client connected", "ip", req.RemoteAddr)

	sendCh := make(chan []byte, 16)
	clientsMu.Lock()
	clients[ws] = sendCh
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		delete(clients, ws)
		clientsMu.Unlock()
	}()

	pongWait := 10 * time.Second
	pingInterval := 1 * time.Minute

	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(pingInterval + pongWait))
		return nil
	})
	ws.SetReadDeadline(time.Now().Add(pingInterval + pongWait))

	// single writer goroutine, all writes go through sendCh or the ping ticker
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case msg, ok := <-sendCh:
				if !ok {
					return
				}
				if err := ws.WriteMessage(websocket.TextMessage, msg); err != nil {
					slog.Error("error writing message to client", "error", err.Error())
					return
				}
			case <-ticker.C:
				if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
					slog.Error("error sending ping to client", "error", err.Error())
					return
				}
			}
		}
	}()

	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			slog.Error("client disconnected", "error", err.Error())
			return
		}
	}
}

func codePost(w http.ResponseWriter, req *http.Request) {
	// guard clauses for unauthorized/improper reqs
	if req.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		io.WriteString(w, "method not allowed")
		return
	} else if req.Header.Get("X-Secret-Token") != sharedSecretToken {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "unauthorized")
		return
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		slog.Error("error reading req body on /code", "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "internal server error")
		return
	}

	code := string(body)

	res, err := stmtInsertCode.Exec(code)
	if err != nil {
		slog.Error("error inserting code into database", "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "internal server error")
		return
	}

	rows, _ := res.RowsAffected()
	if rows == 0 {
		slog.Info("duplicate code received, ignoring", "code", code)
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, "code already sent")
		return
	}

	slog.Info("code received, broadcasting to all clients", "code", code)

	broadcastMessage(body)

	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "code received")
}

func main() {
	flag.Parse()
	slog.Info("starting unwrapped server...")

	// .env is optional -- in Docker, env vars come from the container
	if err := godotenv.Load(); err != nil {
		slog.Info("no .env file found, using environment variables")
	}

	sharedPublicToken = os.Getenv("SHARED_PUBLIC_TOKEN")
	sharedSecretToken = os.Getenv("SHARED_SECRET_TOKEN")

	db, err := sql.Open("sqlite", "unwrapped.db")
	if err != nil {
		slog.Error("error opening database", "error", err.Error())
		os.Exit(1)
	}
	defer db.Close()

	// single connection is fine for this workload and avoids lock contention
	db.SetMaxOpenConns(1)

	_, err = db.Exec("PRAGMA journal_mode=WAL")
	if err != nil {
		slog.Error("error setting WAL mode", "error", err.Error())
		os.Exit(1)
	}

	_, err = db.Exec("CREATE TABLE IF NOT EXISTS sent_codes (code TEXT PRIMARY KEY)")
	if err != nil {
		slog.Error("error creating sent_codes table", "error", err.Error())
		os.Exit(1)
	}

	stmtInsertCode, err = db.Prepare("INSERT OR IGNORE INTO sent_codes (code) VALUES (?)")
	if err != nil {
		slog.Error("error preparing insert statement", "error", err.Error())
		os.Exit(1)
	}
	defer stmtInsertCode.Close()

	slog.Info("~~~ IMPORTANT ~~~\nYOUR SHARED PUBLIC TOKEN IS: " + sharedPublicToken)
	slog.Info("TO SHARE THIS SERVER CONNECTION WITH OTHERS, THEY MUST INPUT THIS TOKEN INTO THEIR CLIENT!")

	http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, "unwrapped is ok")
	})
	http.HandleFunc("/code", codePost)
	http.HandleFunc("/ws", websocketHandler)

	// will be proxied through nginx anyways
	slog.Info("server is running on port " + *addr)
	http.ListenAndServe(*addr, nil)
}
