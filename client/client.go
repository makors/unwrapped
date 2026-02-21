package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

var (
	serverAddr = flag.String("server", "ws.makors.xyz", "websocket server address")
	useTLS     = flag.Bool("tls", false, "use wss:// instead of ws://")
)

const httpSMSEndpoint = "https://api.httpsms.com/v1/messages/send"

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        2,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	},
}

type smsRequest struct {
	Content string `json:"content"`
	From    string `json:"from"`
	To      string `json:"to"`
}

func sendSMS(apiKey, from, to string, message []byte) {
	body, err := json.Marshal(smsRequest{
		Content: string(message),
		From:    from,
		To:      to,
	})
	if err != nil {
		slog.Error("failed to marshal SMS payload", "error", err.Error())
		return
	}

	req, err := http.NewRequest("POST", httpSMSEndpoint, bytes.NewReader(body))
	if err != nil {
		slog.Error("failed to create httpSMS request", "error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Error("failed to send SMS via httpSMS", "error", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		slog.Error("httpSMS returned error", "status", resp.StatusCode, "body", string(respBody))
		return
	}

	io.Copy(io.Discard, resp.Body)
	slog.Info("SMS sent via httpSMS", "to", to, "status", resp.StatusCode)
}

func main() {
	flag.Parse()

	err := godotenv.Load()
	if err != nil {
		slog.Error("error loading .env file")
		os.Exit(1)
	}

	httpSMSKey := os.Getenv("HTTPSMS_API_KEY")
	httpSMSFrom := os.Getenv("HTTPSMS_FROM")
	httpSMSTo := os.Getenv("HTTPSMS_TO")
	if httpSMSKey == "" || httpSMSFrom == "" || httpSMSTo == "" {
		slog.Error("HTTPSMS_API_KEY, HTTPSMS_FROM, and HTTPSMS_TO env vars are required")
		os.Exit(1)
	}

	scheme := "ws"
	if *useTLS {
		scheme = "wss"
	}

	u := url.URL{Scheme: scheme, Host: *serverAddr, Path: "/ws"}
	header := http.Header{}
	header.Set("X-Public-Token", os.Getenv("SHARED_PUBLIC_TOKEN"))

	slog.Info("connecting to server", "url", u.String())

	ws, _, err := websocket.DefaultDialer.Dial(u.String(), header)
	if err != nil {
		slog.Error("failed to connect to server", "error", err.Error())
		os.Exit(1)
	}
	defer ws.Close()

	slog.Info("connected to server")

	// server pings us every minute, we need to respond within 10s
	// gorilla/websocket responds to pings automatically, but we set
	// a read deadline so we notice if the server disappears
	ws.SetReadDeadline(time.Now().Add(80 * time.Second))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(80 * time.Second))
		return nil
	})

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			_, message, err := ws.ReadMessage()
			if err != nil {
				slog.Error("read error, disconnected", "error", err.Error())
				return
			}
			slog.Info("received message", "size", len(message))
			go sendSMS(httpSMSKey, httpSMSFrom, httpSMSTo, message)
		}
	}()

	select {
	case <-done:
		slog.Info("connection closed")
	case <-interrupt:
		slog.Info("interrupted, closing connection...")
		err := ws.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
		if err != nil {
			slog.Error("error sending close message", "error", err.Error())
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}
