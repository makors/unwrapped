package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

var (
	serverAddr = flag.String("server", "ws.makors.xyz", "websocket server address")
	useTLS     = flag.Bool("tls", true, "use wss:// instead of ws://")
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

type discordPayload struct {
	Content string `json:"content"`
}

func sendSMS(apiKey, from, to string, message string) {
	body, err := json.Marshal(smsRequest{
		Content: message,
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

func sendIMessage(to string, message string) {
	script := fmt.Sprintf(`tell application "Messages"
	set targetService to 1st account whose service type = SMS
	set targetBuddy to participant %q of targetService
	send %q to targetBuddy
end tell`, to, message)

	cmd := exec.Command("osascript", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("failed to send iMessage", "error", err.Error(), "output", string(out))
		return
	}
	slog.Info("iMessage sent", "to", to)
}

func sendDiscordWebhook(webhookURL string, code string) {
	content := fmt.Sprintf("# [CLICK HERE FOR FREE CHIPOTLE](sms://888222?body=%s)", code)
	body, err := json.Marshal(discordPayload{Content: content})
	if err != nil {
		slog.Error("failed to marshal discord payload", "error", err.Error())
		return
	}

	resp, err := httpClient.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Error("failed to send discord webhook", "error", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Error("discord webhook returned error", "status", resp.StatusCode)
		return
	}
	slog.Info("discord webhook sent", "status", resp.StatusCode)
}

func main() {
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		slog.Error("error loading .env file")
		os.Exit(1)
	}

	httpSMSKey := os.Getenv("HTTPSMS_API_KEY")
	httpSMSFrom := os.Getenv("HTTPSMS_FROM")
	httpSMSTo := os.Getenv("HTTPSMS_TO")
	smsEnabled := httpSMSKey != "" && httpSMSFrom != "" && httpSMSTo != ""

	imessageTo := os.Getenv("IMESSAGE_TO")
	discordWebhookURL := os.Getenv("DISCORD_WEBHOOK_URL")

	if !smsEnabled && imessageTo == "" && discordWebhookURL == "" {
		slog.Error("no delivery methods configured; set HTTPSMS_*, IMESSAGE_TO, and/or DISCORD_WEBHOOK_URL")
		os.Exit(1)
	}

	if smsEnabled {
		slog.Info("httpSMS enabled", "from", httpSMSFrom, "to", httpSMSTo)
	}
	if imessageTo != "" {
		slog.Info("iMessage enabled", "to", imessageTo)
	}
	if discordWebhookURL != "" {
		slog.Info("Discord webhook enabled")
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

	const (
		pingInterval = 30 * time.Second
		pongWait     = 10 * time.Second
	)
	readDeadline := pingInterval + pongWait

	ws.SetReadDeadline(time.Now().Add(readDeadline))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(readDeadline))
		return nil
	})

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	done := make(chan struct{})
	closeCh := make(chan struct{}, 1)

	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-closeCh:
				ws.WriteMessage(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				)
				return
			case <-ticker.C:
				if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
					slog.Error("ping error", "error", err.Error())
					return
				}
			case <-done:
				return
			}
		}
	}()

	go func() {
		defer close(done)
		for {
			_, message, err := ws.ReadMessage()
			if err != nil {
				slog.Error("read error, disconnected", "error", err.Error())
				return
			}

			msg := string(message)
			slog.Info("received message", "size", len(message))

			if smsEnabled {
				go sendSMS(httpSMSKey, httpSMSFrom, httpSMSTo, msg)
			}
			if imessageTo != "" {
				go sendIMessage(imessageTo, msg)
			}
			if discordWebhookURL != "" {
				go sendDiscordWebhook(discordWebhookURL, msg)
			}
		}
	}()

	select {
	case <-done:
		slog.Info("connection closed")
	case <-interrupt:
		slog.Info("interrupted, closing connection...")
		closeCh <- struct{}{}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}
