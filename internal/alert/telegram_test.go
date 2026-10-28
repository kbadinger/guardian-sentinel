package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "123456:test-token-not-real"

func TestTelegramSend(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := &Telegram{Token: testToken, ChatID: "42", API: srv.URL, Client: srv.Client()}
	text := "🚨 sentinel[fleet]: site DOWN — 3 consecutive failures (connection refused). 12:41 ET"
	if err := tg.Send(context.Background(), text); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bot"+testToken+"/sendMessage" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["chat_id"] != "42" || gotBody["text"] != text {
		t.Errorf("body = %v", gotBody)
	}
	if _, ok := gotBody["parse_mode"]; ok {
		t.Error("messages are plain text; no parse_mode")
	}
}

func TestTelegramErrors(t *testing.T) {
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer rejecting.Close()

	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close() // nothing listens there any more

	tests := []struct {
		name    string
		api     string
		wantErr string
	}{
		{"telegram rejects", rejecting.URL, "HTTP 400: Bad Request: chat not found"},
		{"cannot connect", closedURL, "telegram:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tg := &Telegram{Token: testToken, ChatID: "42", API: tt.api}
			err := tg.Send(context.Background(), "hello")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("error leaks the bot token: %q", err)
			}
		})
	}
}

func TestNewTelegramFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr []string
	}{
		{"both set", map[string]string{EnvBotToken: testToken, EnvChatID: "42"}, nil},
		{"token missing", map[string]string{EnvChatID: "42"}, []string{EnvBotToken}},
		{"both missing", map[string]string{}, []string{EnvBotToken, EnvChatID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tg, err := NewTelegramFromEnv(func(k string) string { return tt.env[k] })
			if tt.wantErr == nil {
				if err != nil || tg.Token != testToken || tg.ChatID != "42" {
					t.Fatalf("got %+v, %v", tg, err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
			if strings.Contains(err.Error(), "42") {
				t.Errorf("error prints a value: %q", err)
			}
		})
	}
}

func TestPrinter(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{W: &buf}
	if err := p.Send(context.Background(), "sentinel[fleet] online — 3/3 ✓"); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "[dry-run] sentinel[fleet] online — 3/3 ✓\n" {
		t.Errorf("printed %q", got)
	}
}
