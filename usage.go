package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// UsageLogger records per-request metrics to a SQLite database.
// All methods are safe for concurrent use.
type UsageLogger struct {
	db     *sql.DB
	readDB *sql.DB
	mu     sync.Mutex
}

// UsageRecord holds the metrics for a single proxied request.
type UsageRecord struct {
	Timestamp     time.Time
	KeyHash       string // first 16 hex chars of SHA-256(key)
	KeyName       string
	Model         string
	Endpoint      string
	StatusCode    int
	RequestBytes  int64
	ResponseBytes int64
	InputTokens   int
	OutputTokens  int
	TotalTokens   int
	DurationMS    int64
}

const schema = `
CREATE TABLE IF NOT EXISTS usage (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	timestamp     TEXT    NOT NULL,
	key_hash      TEXT    NOT NULL,
	key_name      TEXT    NOT NULL DEFAULT '',
	model         TEXT    NOT NULL,
	endpoint      TEXT    NOT NULL DEFAULT '',
	status_code   INTEGER NOT NULL DEFAULT 0,
	request_bytes INTEGER NOT NULL DEFAULT 0,
	response_bytes INTEGER NOT NULL DEFAULT 0,
	input_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens  INTEGER NOT NULL DEFAULT 0,
	duration_ms   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_usage_timestamp ON usage(timestamp);
CREATE INDEX IF NOT EXISTS idx_usage_key_hash  ON usage(key_hash);
CREATE INDEX IF NOT EXISTS idx_usage_model     ON usage(model);
`

// NewUsageLogger opens (or creates) the SQLite database at the given path.
func NewUsageLogger(dbPath string) (*UsageLogger, error) {
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening usage db: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating usage schema: %w", err)
	}

	readDB, err := sql.Open("sqlite", dbPath+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("opening usage read db: %w", err)
	}
	readDB.SetMaxOpenConns(1)

	return &UsageLogger{db: db, readDB: readDB}, nil
}

// Log records a usage entry. Non-blocking failures are logged but do not
// propagate to the caller — usage logging must never break request handling.
func (ul *UsageLogger) Log(rec UsageRecord) {
	ul.mu.Lock()
	defer ul.mu.Unlock()

	_, err := ul.db.Exec(`
		INSERT INTO usage (timestamp, key_hash, key_name, model, endpoint,
			status_code, request_bytes, response_bytes,
			input_tokens, output_tokens, total_tokens, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Timestamp.UTC().Format(time.RFC3339),
		rec.KeyHash,
		rec.KeyName,
		rec.Model,
		rec.Endpoint,
		rec.StatusCode,
		rec.RequestBytes,
		rec.ResponseBytes,
		rec.InputTokens,
		rec.OutputTokens,
		rec.TotalTokens,
		rec.DurationMS,
	)
	if err != nil {
		slog.Error("failed to log usage", "error", err)
	}
}

// Close closes the underlying database.
func (ul *UsageLogger) Close() error {
	err1 := ul.readDB.Close()
	err2 := ul.db.Close()
	if err2 != nil {
		return err2
	}
	return err1
}

type DashboardData struct {
	Totals      DashboardTotals `json:"totals"`
	Daily       []DailyRow      `json:"daily"`
	DailyModels []DailyModelRow `json:"daily_models"`
	Users       []UserRow       `json:"users"`
	Models      []ModelRow      `json:"models"`
}

type DashboardTotals struct {
	Requests    int     `json:"requests"`
	TotalTokens int64   `json:"total_tokens"`
	Users       int     `json:"users"`
	ErrorRate   float64 `json:"error_rate"`
}

type DailyRow struct {
	Date        string `json:"date"`
	Requests    int    `json:"requests"`
	TotalTokens int64  `json:"total_tokens"`
	Errors      int    `json:"errors"`
}

type DailyModelRow struct {
	Date        string `json:"date"`
	Model       string `json:"model"`
	Requests    int    `json:"requests"`
	TotalTokens int64  `json:"total_tokens"`
}

type UserRow struct {
	Name        string `json:"name"`
	KeyHash     string `json:"key_hash"`
	Requests    int    `json:"requests"`
	TotalTokens int64  `json:"total_tokens"`
	ActiveDays  int    `json:"active_days"`
	LastSeen    string `json:"last_seen"`
}

type ModelRow struct {
	Model       string  `json:"model"`
	Requests    int     `json:"requests"`
	Users       int     `json:"users"`
	TotalTokens int64   `json:"total_tokens"`
	AvgLatency  float64 `json:"avg_latency_ms"`
}

func (ul *UsageLogger) QueryDashboardData(days int) (*DashboardData, error) {
	if days <= 0 {
		days = 30
	}
	periodArg := fmt.Sprintf("-%d days", days)

	var data DashboardData

	err := ul.readDB.QueryRow(`
		SELECT
			COUNT(*),
			COALESCE(SUM(total_tokens), 0),
			COUNT(DISTINCT key_hash),
			CAST(COALESCE(100.0 * SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END) / NULLIF(COUNT(*), 0), 0) AS REAL)
		FROM usage
		WHERE timestamp >= date('now', ?)
	`, periodArg).Scan(&data.Totals.Requests, &data.Totals.TotalTokens, &data.Totals.Users, &data.Totals.ErrorRate)
	if err != nil {
		return nil, fmt.Errorf("totals query: %w", err)
	}

	rows, err := ul.readDB.Query(`
		SELECT
			date(timestamp) AS day,
			COUNT(*)        AS requests,
			COALESCE(SUM(total_tokens), 0),
			SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END) AS errors
		FROM usage
		WHERE timestamp >= date('now', ?)
		GROUP BY day
		ORDER BY day
	`, periodArg)
	if err != nil {
		return nil, fmt.Errorf("daily query: %w", err)
	}
	for rows.Next() {
		var r DailyRow
		if err := rows.Scan(&r.Date, &r.Requests, &r.TotalTokens, &r.Errors); err != nil {
			continue
		}
		data.Daily = append(data.Daily, r)
	}
	rows.Close()

	userRows, err := ul.readDB.Query(`
		SELECT
			key_name,
			key_hash,
			COUNT(*)        AS requests,
			COALESCE(SUM(total_tokens), 0),
			COUNT(DISTINCT date(timestamp)) AS active_days,
			MAX(timestamp)  AS last_seen
		FROM usage
		WHERE timestamp >= date('now', ?)
		GROUP BY key_hash
		ORDER BY total_tokens DESC
	`, periodArg)
	if err != nil {
		return nil, fmt.Errorf("users query: %w", err)
	}
	for userRows.Next() {
		var r UserRow
		var lastSeen string
		if err := userRows.Scan(&r.Name, &r.KeyHash, &r.Requests, &r.TotalTokens, &r.ActiveDays, &lastSeen); err != nil {
			continue
		}
		if r.Name == "" {
			r.Name = "(unnamed)"
		}
		if len(r.KeyHash) > 8 {
			r.KeyHash = r.KeyHash[:8]
		}
		if len(lastSeen) >= 10 {
			r.LastSeen = lastSeen[:10]
		} else {
			r.LastSeen = lastSeen
		}
		data.Users = append(data.Users, r)
	}
	userRows.Close()

	modelRows, err := ul.readDB.Query(`
		SELECT
			model,
			COUNT(*)        AS requests,
			COUNT(DISTINCT key_hash) AS unique_users,
			COALESCE(SUM(total_tokens), 0),
			CAST(AVG(duration_ms) AS REAL) AS avg_duration_ms
		FROM usage
		WHERE timestamp >= date('now', ?)
		GROUP BY model
		ORDER BY total_tokens DESC
	`, periodArg)
	if err != nil {
		return nil, fmt.Errorf("models query: %w", err)
	}
	for modelRows.Next() {
		var r ModelRow
		if err := modelRows.Scan(&r.Model, &r.Requests, &r.Users, &r.TotalTokens, &r.AvgLatency); err != nil {
			continue
		}
		data.Models = append(data.Models, r)
	}
	modelRows.Close()

	dailyModelRows, err := ul.readDB.Query(`
		SELECT
			date(timestamp) AS day,
			model,
			COUNT(*)        AS requests,
			COALESCE(SUM(total_tokens), 0)
		FROM usage
		WHERE timestamp >= date('now', ?)
		GROUP BY day, model
		ORDER BY day, model
	`, periodArg)
	if err != nil {
		return nil, fmt.Errorf("daily models query: %w", err)
	}
	for dailyModelRows.Next() {
		var r DailyModelRow
		if err := dailyModelRows.Scan(&r.Date, &r.Model, &r.Requests, &r.TotalTokens); err != nil {
			continue
		}
		data.DailyModels = append(data.DailyModels, r)
	}
	dailyModelRows.Close()

	return &data, nil
}

// HashKey returns the first 16 hex characters of the SHA-256 hash of a key.
// This is enough to uniquely identify keys without leaking the secret.
func HashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:8])
}

// --- Token extraction from upstream responses ---

// TokenUsage holds token counts extracted from an upstream response.
type TokenUsage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// ExtractTokenUsage attempts to extract token counts from a response body.
// It handles both OpenAI and Anthropic response formats, as well as SSE
// streaming responses from both providers.
func ExtractTokenUsage(responseBody []byte, backendType string, isStreaming bool) TokenUsage {
	if isStreaming {
		return extractTokensFromSSE(responseBody, backendType)
	}
	return extractTokensFromJSON(responseBody, backendType)
}

// extractTokensFromJSON handles non-streaming responses.
func extractTokensFromJSON(body []byte, backendType string) TokenUsage {
	if backendType == BackendAnthropic {
		return extractAnthropicTokens(body)
	}
	return extractOpenAITokens(body)
}

// extractOpenAITokens parses the "usage" object from an OpenAI-format response.
//
//	{"usage": {"prompt_tokens": N, "completion_tokens": N, "total_tokens": N}}
func extractOpenAITokens(body []byte) TokenUsage {
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return TokenUsage{}
	}
	return TokenUsage{
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
		TotalTokens:  resp.Usage.TotalTokens,
	}
}

// extractAnthropicTokens parses the "usage" object from an Anthropic-format response.
//
//	{"usage": {"input_tokens": N, "output_tokens": N, "cache_creation_input_tokens": N, "cache_read_input_tokens": N}}
func extractAnthropicTokens(body []byte) TokenUsage {
	var resp struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return TokenUsage{}
	}
	total := resp.Usage.InputTokens + resp.Usage.OutputTokens +
		resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens
	return TokenUsage{
		InputTokens:  resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  total,
	}
}

// extractTokensFromSSE scans SSE data lines for token usage information.
// OpenAI puts usage in the final chunk; Anthropic sends it in message_delta
// and message_start events.
func extractTokensFromSSE(body []byte, backendType string) TokenUsage {
	if backendType == BackendAnthropic {
		return extractAnthropicSSETokens(body)
	}
	return extractOpenAISSETokens(body)
}

// extractOpenAISSETokens scans for the usage object in the final SSE chunk.
// OpenAI sends: data: {"usage":{"prompt_tokens":N,...},"choices":[]}
func extractOpenAISSETokens(body []byte) TokenUsage {
	var best TokenUsage
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		if data == "[DONE]" {
			continue
		}
		u := extractOpenAITokens([]byte(data))
		if u.TotalTokens > 0 {
			best = u
		}
	}
	return best
}

// extractAnthropicSSETokens scans Anthropic SSE events for token usage.
// message_start contains input_tokens; message_delta contains output_tokens.
func extractAnthropicSSETokens(body []byte) TokenUsage {
	var usage TokenUsage
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	var currentEvent string
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimSpace(line[7:])
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := []byte(line[6:])

		switch currentEvent {
		case "message_start":
			// {"type":"message_start","message":{"usage":{"input_tokens":N}}}
			var msg struct {
				Message struct {
					Usage struct {
						InputTokens              int `json:"input_tokens"`
						CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
						CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(data, &msg) == nil {
				usage.InputTokens = msg.Message.Usage.InputTokens +
					msg.Message.Usage.CacheCreationInputTokens +
					msg.Message.Usage.CacheReadInputTokens
			}

		case "message_delta":
			// {"type":"message_delta","usage":{"output_tokens":N}}
			var msg struct {
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &msg) == nil {
				usage.OutputTokens = msg.Usage.OutputTokens
			}
		}
	}

	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	return usage
}
