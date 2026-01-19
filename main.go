package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	rdb *redis.Client
	ctx = context.Background()
)

// Known agents for cleanup
var knownAgents = []string{"claude-code", "claude", "opencode", "cursor", "windsurf", "other"}

func main() {
	// Initialize Redis
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379"
	}

	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("Failed to parse Redis URL: %v", err)
	}
	rdb = redis.NewClient(opt)

	// Test Redis connection
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	log.Println("Connected to Redis")

	// Start background cleanup
	go cleanupLoop()

	// Setup HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /heartbeat", handleHeartbeat)
	mux.HandleFunc("POST /vibes", handleVibes)
	mux.HandleFunc("GET /pulse", handlePulse)

	// CORS middleware
	handler := corsMiddleware(mux)

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	server := &http.Server{
		Addr:    ":" + port,
		Handler: handler,
	}

	// Graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down...")
		_ = server.Shutdown(ctx)
	}()

	log.Printf("Server starting on port %s", port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// GET /health
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// HeartbeatRequest is the request body for POST /heartbeat
type HeartbeatRequest struct {
	ClientID string `json:"c"`
	Agent    string `json:"a"`
}

// HeartbeatResponse is the response for POST /heartbeat
type HeartbeatResponse struct {
	Count int64 `json:"n"`
}

// POST /heartbeat
func handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if req.ClientID == "" || req.Agent == "" {
		http.Error(w, "missing c or a", http.StatusBadRequest)
		return
	}

	agent := sanitizeAgent(req.Agent)
	minute := time.Now().Unix() / 60
	key := fmt.Sprintf("vibes:hll:%s:%d", agent, minute)

	// Add to HyperLogLog and set TTL
	pipe := rdb.Pipeline()
	pipe.PFAdd(ctx, key, req.ClientID)
	pipe.Expire(ctx, key, 5*time.Minute)
	_, _ = pipe.Exec(ctx)

	// Count active users (last 5 minutes)
	count := countActiveUsers(agent)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(HeartbeatResponse{Count: count})
}

// VibesRequest is the request body for POST /vibes
type VibesRequest struct {
	ClientID string `json:"client_id"`
	Agent    string `json:"agent"`
	Message  string `json:"message,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// Drop represents a single vibe drop
type Drop struct {
	Message string `json:"m"`
	TimeAgo string `json:"t"`
}

// VibesResponse is the response for POST /vibes
type VibesResponse struct {
	Drops       []Drop `json:"drops"`
	Count       int64  `json:"n"`
	OK          bool   `json:"ok"`
	WeeklyDrops int64  `json:"w"`
}

// POST /vibes
func handleVibes(w http.ResponseWriter, r *http.Request) {
	var req VibesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if req.ClientID == "" || req.Agent == "" {
		http.Error(w, "missing client_id or agent", http.StatusBadRequest)
		return
	}

	agent := sanitizeAgent(req.Agent)
	limit := req.Limit
	if limit <= 0 || limit > 10 {
		limit = 5
	}

	var posted bool

	// If message provided, try to post it
	if req.Message != "" {
		msg := strings.TrimSpace(req.Message)
		if len(msg) > 140 {
			msg = msg[:140]
		}

		if msg != "" {
			// Check rate limit
			rateLimitKey := fmt.Sprintf("vibes:r:%s", req.ClientID)
			count, _ := rdb.Get(ctx, rateLimitKey).Int()

			if count < 5 {
				// Increment rate limit
				pipe := rdb.Pipeline()
				pipe.Incr(ctx, rateLimitKey)
				pipe.Expire(ctx, rateLimitKey, time.Hour)
				_, _ = pipe.Exec(ctx)

				// Store drop
				ts := time.Now().UnixMilli()
				dropJSON, _ := json.Marshal(map[string]interface{}{
					"m":  msg,
					"ts": ts,
				})
				dropKey := fmt.Sprintf("vibes:d:%s", agent)
				rdb.ZAdd(ctx, dropKey, redis.Z{
					Score:  float64(ts),
					Member: string(dropJSON),
				})

				// Increment global weekly counter
				statsKey := weeklyStatsKey()
				pipe = rdb.Pipeline()
				pipe.Incr(ctx, statsKey)
				pipe.Expire(ctx, statsKey, 14*24*time.Hour) // 2 week TTL
				_, _ = pipe.Exec(ctx)

				posted = true
			}
		}
	}

	// Get recent drops
	dropKey := fmt.Sprintf("vibes:d:%s", agent)
	results, _ := rdb.ZRevRange(ctx, dropKey, 0, int64(limit-1)).Result()

	drops := make([]Drop, 0, len(results))
	now := time.Now()

	for _, result := range results {
		var data struct {
			Message   string `json:"m"`
			Timestamp int64  `json:"ts"`
		}
		if err := json.Unmarshal([]byte(result), &data); err != nil {
			continue
		}

		drops = append(drops, Drop{
			Message: data.Message,
			TimeAgo: formatTimeAgo(now, data.Timestamp),
		})
	}

	// Get active count
	count := countActiveUsers(agent)

	// Get weekly drops count (global, cross-agent)
	weeklyDrops, _ := rdb.Get(ctx, weeklyStatsKey()).Int64()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(VibesResponse{
		Drops:       drops,
		Count:       count,
		OK:          posted,
		WeeklyDrops: weeklyDrops,
	})
}

// PulseResponse is the response for GET /pulse
type PulseResponse struct {
	Count int64 `json:"n"`
}

// GET /pulse?a={agent}
func handlePulse(w http.ResponseWriter, r *http.Request) {
	agent := sanitizeAgent(r.URL.Query().Get("a"))
	if agent == "" {
		agent = "other"
	}

	count := countActiveUsers(agent)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(PulseResponse{Count: count})
}

// countActiveUsers returns the approximate count of active users in the last 5 minutes
func countActiveUsers(agent string) int64 {
	now := time.Now().Unix() / 60
	keys := make([]string, 5)
	for i := 0; i < 5; i++ {
		keys[i] = fmt.Sprintf("vibes:hll:%s:%d", agent, now-int64(i))
	}
	count, _ := rdb.PFCount(ctx, keys...).Result()
	return count
}

// sanitizeAgent ensures agent is a known value
func sanitizeAgent(agent string) string {
	agent = strings.ToLower(strings.TrimSpace(agent))
	for _, known := range knownAgents {
		if agent == known {
			return agent
		}
	}
	return "other"
}

// weeklyStatsKey returns the Redis key for weekly drop counter
func weeklyStatsKey() string {
	year, week := time.Now().ISOWeek()
	return fmt.Sprintf("vibes:stats:week:%d:%d", year, week)
}

// formatTimeAgo formats a timestamp as relative time
func formatTimeAgo(now time.Time, tsMs int64) string {
	ts := time.UnixMilli(tsMs)
	diff := now.Sub(ts)

	if diff < time.Minute {
		return "now"
	}
	if diff < time.Hour {
		return strconv.Itoa(int(diff.Minutes())) + "m"
	}
	if diff < 24*time.Hour {
		return strconv.Itoa(int(diff.Hours())) + "h"
	}
	return strconv.Itoa(int(diff.Hours()/24)) + "d"
}

// cleanupLoop runs periodic cleanup of old drops
func cleanupLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()
		for _, agent := range knownAgents {
			key := fmt.Sprintf("vibes:d:%s", agent)
			rdb.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(cutoff, 10))
		}
	}
}
