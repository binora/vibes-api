package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// setupTestRedis creates a miniredis instance and configures the global rdb client
func setupTestRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}
	rdb = redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})
	return mr
}

// Unit Tests for Pure Functions

func TestSanitizeAgent(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"known agent claude-code", "claude-code", "claude-code"},
		{"known agent opencode", "opencode", "opencode"},
		{"known agent cursor", "cursor", "cursor"},
		{"known agent windsurf", "windsurf", "windsurf"},
		{"known agent other", "other", "other"},
		{"unknown agent", "unknown", "other"},
		{"case insensitive", "CLAUDE-CODE", "claude-code"},
		{"mixed case", "ClAuDe-CoDe", "claude-code"},
		{"with leading whitespace", "  claude-code", "claude-code"},
		{"with trailing whitespace", "claude-code  ", "claude-code"},
		{"with both whitespace", "  cursor  ", "cursor"},
		{"empty string", "", "other"},
		{"random string", "vscode", "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeAgent(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeAgent(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestWeeklyStatsKey(t *testing.T) {
	key := weeklyStatsKey()

	// Verify the format is correct
	if key == "" {
		t.Error("weeklyStatsKey() returned empty string")
	}
	if !bytes.HasPrefix([]byte(key), []byte("vibes:stats:week:")) {
		t.Errorf("weeklyStatsKey() = %q, want prefix 'vibes:stats:week:'", key)
	}
}

func TestFormatTimeAgo(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		tsMs     int64
		expected string
	}{
		{"just now (0 seconds)", now.UnixMilli(), "now"},
		{"30 seconds ago", now.Add(-30 * time.Second).UnixMilli(), "now"},
		{"1 minute ago", now.Add(-1 * time.Minute).UnixMilli(), "1m"},
		{"5 minutes ago", now.Add(-5 * time.Minute).UnixMilli(), "5m"},
		{"59 minutes ago", now.Add(-59 * time.Minute).UnixMilli(), "59m"},
		{"1 hour ago", now.Add(-1 * time.Hour).UnixMilli(), "1h"},
		{"2 hours ago", now.Add(-2 * time.Hour).UnixMilli(), "2h"},
		{"23 hours ago", now.Add(-23 * time.Hour).UnixMilli(), "23h"},
		{"1 day ago", now.Add(-24 * time.Hour).UnixMilli(), "1d"},
		{"3 days ago", now.Add(-72 * time.Hour).UnixMilli(), "3d"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatTimeAgo(now, tt.tsMs)
			if result != tt.expected {
				t.Errorf("formatTimeAgo() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// Integration Tests for HTTP Handlers

func TestHandleHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
}

func TestHandleHeartbeat(t *testing.T) {
	mr := setupTestRedis(t)
	defer mr.Close()

	t.Run("valid request", func(t *testing.T) {
		body := `{"c": "client-123", "a": "claude-code"}`
		req := httptest.NewRequest(http.MethodPost, "/heartbeat", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handleHeartbeat(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var resp HeartbeatResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
	})

	t.Run("missing client_id", func(t *testing.T) {
		body := `{"a": "claude-code"}`
		req := httptest.NewRequest(http.MethodPost, "/heartbeat", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handleHeartbeat(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing agent", func(t *testing.T) {
		body := `{"c": "client-123"}`
		req := httptest.NewRequest(http.MethodPost, "/heartbeat", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handleHeartbeat(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		body := `not json`
		req := httptest.NewRequest(http.MethodPost, "/heartbeat", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handleHeartbeat(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})
}

func TestHandleVibes(t *testing.T) {
	mr := setupTestRedis(t)
	defer mr.Close()

	t.Run("get drops without posting", func(t *testing.T) {
		body := `{"client_id": "client-123", "agent": "claude-code"}`
		req := httptest.NewRequest(http.MethodPost, "/vibes", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handleVibes(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var resp VibesResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if resp.OK {
			t.Error("OK should be false when no message posted")
		}
	})

	t.Run("post a drop", func(t *testing.T) {
		body := `{"client_id": "client-456", "agent": "claude-code", "message": "hello world"}`
		req := httptest.NewRequest(http.MethodPost, "/vibes", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handleVibes(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var resp VibesResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if !resp.OK {
			t.Error("OK should be true when message posted")
		}

		if len(resp.Drops) == 0 {
			t.Error("drops should contain the posted message")
		}
	})

	t.Run("message truncation at 140 chars", func(t *testing.T) {
		longMessage := ""
		for i := 0; i < 200; i++ {
			longMessage += "x"
		}
		body := `{"client_id": "client-789", "agent": "claude-code", "message": "` + longMessage + `"}`
		req := httptest.NewRequest(http.MethodPost, "/vibes", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handleVibes(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var resp VibesResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		// Find our message in drops
		for _, drop := range resp.Drops {
			if len(drop.Message) > 140 {
				t.Errorf("message length = %d, want <= 140", len(drop.Message))
			}
		}
	})

	t.Run("custom limit parameter", func(t *testing.T) {
		body := `{"client_id": "client-123", "agent": "claude-code", "limit": 2}`
		req := httptest.NewRequest(http.MethodPost, "/vibes", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handleVibes(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})

	t.Run("rate limiting", func(t *testing.T) {
		clientID := "rate-limit-client"
		mr.FlushAll() // Clear previous state

		// Post 5 messages (should succeed)
		for i := 0; i < 5; i++ {
			body := `{"client_id": "` + clientID + `", "agent": "claude-code", "message": "msg"}`
			req := httptest.NewRequest(http.MethodPost, "/vibes", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handleVibes(w, req)

			var resp VibesResponse
			_ = json.NewDecoder(w.Body).Decode(&resp)
			if !resp.OK {
				t.Errorf("message %d should have been posted", i+1)
			}
		}

		// 6th message should fail due to rate limit
		body := `{"client_id": "` + clientID + `", "agent": "claude-code", "message": "msg6"}`
		req := httptest.NewRequest(http.MethodPost, "/vibes", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handleVibes(w, req)

		var resp VibesResponse
		_ = json.NewDecoder(w.Body).Decode(&resp)
		if resp.OK {
			t.Error("6th message should have been rate limited")
		}
	})

	t.Run("missing client_id", func(t *testing.T) {
		body := `{"agent": "claude-code"}`
		req := httptest.NewRequest(http.MethodPost, "/vibes", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handleVibes(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("weekly counter increments on post", func(t *testing.T) {
		mr.FlushAll()

		// First request - get baseline
		body := `{"client_id": "weekly-test", "agent": "claude-code"}`
		req := httptest.NewRequest(http.MethodPost, "/vibes", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handleVibes(w, req)

		var resp1 VibesResponse
		_ = json.NewDecoder(w.Body).Decode(&resp1)
		initialCount := resp1.WeeklyDrops

		// Post a message
		body = `{"client_id": "weekly-test", "agent": "claude-code", "message": "test drop"}`
		req = httptest.NewRequest(http.MethodPost, "/vibes", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		handleVibes(w, req)

		var resp2 VibesResponse
		_ = json.NewDecoder(w.Body).Decode(&resp2)

		if resp2.WeeklyDrops != initialCount+1 {
			t.Errorf("weekly count = %d, want %d", resp2.WeeklyDrops, initialCount+1)
		}
	})

	t.Run("weekly counter is global across agents", func(t *testing.T) {
		mr.FlushAll()

		// Post from claude-code
		body := `{"client_id": "agent1", "agent": "claude-code", "message": "from claude"}`
		req := httptest.NewRequest(http.MethodPost, "/vibes", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handleVibes(w, req)

		// Post from cursor
		body = `{"client_id": "agent2", "agent": "cursor", "message": "from cursor"}`
		req = httptest.NewRequest(http.MethodPost, "/vibes", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		handleVibes(w, req)

		var resp VibesResponse
		_ = json.NewDecoder(w.Body).Decode(&resp)

		if resp.WeeklyDrops != 2 {
			t.Errorf("weekly count = %d, want 2 (global across agents)", resp.WeeklyDrops)
		}
	})
}

func TestHandlePulse(t *testing.T) {
	mr := setupTestRedis(t)
	defer mr.Close()

	t.Run("with agent param", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/pulse?a=claude-code", nil)
		w := httptest.NewRecorder()

		handlePulse(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var resp PulseResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
	})

	t.Run("without agent param defaults to other", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/pulse", nil)
		w := httptest.NewRecorder()

		handlePulse(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})
}

func TestCorsMiddleware(t *testing.T) {
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("sets CORS headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Error("missing Access-Control-Allow-Origin header")
		}
		if w.Header().Get("Access-Control-Allow-Methods") != "GET, POST, OPTIONS" {
			t.Error("missing Access-Control-Allow-Methods header")
		}
		if w.Header().Get("Access-Control-Allow-Headers") != "Content-Type" {
			t.Error("missing Access-Control-Allow-Headers header")
		}
	})

	t.Run("OPTIONS returns 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/", nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
	})
}
