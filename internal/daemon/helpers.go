package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func rawJSONMap(input any) map[string]any {
	raw := MustMarshalJSON(input)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded == nil {
		return map[string]any{}
	}
	return decoded
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func addMillis(timestamp string, delta int) string {
	base, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		base = time.Now().UTC()
	}
	return base.Add(time.Duration(delta) * time.Millisecond).UTC().Format(time.RFC3339)
}

func stringFromMap(input map[string]any, key, fallback string) string {
	if input == nil {
		return fallback
	}
	if value, ok := input[key].(string); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func intFromMap(input map[string]any, key string, fallback int) int {
	if input == nil {
		return fallback
	}
	switch typed := input[key].(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float32:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return int(parsed)
		}
	}
	return fallback
}

func stringSliceFromMap(input map[string]any, key string) []string {
	if input == nil {
		return nil
	}
	raw, ok := input[key].([]any)
	if !ok {
		if typed, ok := input[key].([]string); ok {
			return append([]string{}, typed...)
		}
		return nil
	}
	values := make([]string, 0, len(raw))
	for _, value := range raw {
		if text, ok := value.(string); ok && text != "" {
			values = append(values, text)
		}
	}
	return values
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return ""
	}
}

func isOnionPeerURL(peerURL string) bool {
	parsed, err := url.Parse(peerURL)
	if err != nil || parsed.Host == "" {
		return false
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return false
	}
	return isOnionHost(host)
}

func isOnionHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return strings.HasSuffix(host, ".onion") && host != ".onion"
}
