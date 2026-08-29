package nativeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
)

// sensitiveFieldsPartialMask contains configuration field names that should be redacted
// using partial masking (first and last character visible, middle replaced with *).
// For values with 7+ characters: "secretvalue123" becomes "s***********3"
// For values with <7 characters: "short" becomes "****"
// Add field paths using dot notation (e.g., "LastFM.ApiKey", "Spotify.Secret")
var sensitiveFieldsPartialMask = []string{
	"LastFM.ApiKey",
	"LastFM.Secret",
	"Prometheus.MetricsPath",
	"Spotify.ID",
	"Spotify.Secret",
	"DevAutoLoginUsername",
}

// sensitiveFieldsFullMask contains configuration field names that should always be
// completely masked with "****" regardless of their length.
// Add field paths using dot notation for any fields that should never show any content.
var sensitiveFieldsFullMask = []string{
	"DevAutoCreateAdminPassword",
	"PasswordEncryptionKey",
	"Prometheus.Password",
}

// configResponse 是配置查看接口的响应。
type configResponse struct {
	ID         string                 `json:"id"`
	ConfigFile string                 `json:"configFile"`
	Config     map[string]interface{} `json:"config"`
}

// redactValue 按字段名对敏感值脱敏。
// 部分脱敏保留首尾字符，便于用户核对自己填的是哪一个密钥而不泄露内容；
// 过短的值无法安全地保留首尾，一律全遮蔽。
func redactValue(key string, value string) string {
	// Return empty values as-is
	if len(value) == 0 {
		return value
	}

	// Check if this field should be fully masked
	for _, field := range sensitiveFieldsFullMask {
		if field == key {
			return "****"
		}
	}

	// Check if this field should be partially masked
	for _, field := range sensitiveFieldsPartialMask {
		if field == key {
			if len(value) < 7 {
				return "****"
			}
			// Show first and last character with * in between
			return string(value[0]) + strings.Repeat("*", len(value)-2) + string(value[len(value)-1])
		}
	}

	// Return original value if not sensitive
	return value
}

// applySensitiveFieldMasking recursively applies masking to sensitive fields in the configuration map
// applySensitiveFieldMasking 递归遍历配置树做脱敏，字段路径以点号拼接。
func applySensitiveFieldMasking(ctx context.Context, config map[string]interface{}, prefix string) {
	for key, value := range config {
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}

		switch v := value.(type) {
		case map[string]interface{}:
			// Recursively process nested maps
			applySensitiveFieldMasking(ctx, v, fullKey)
		case string:
			// Apply masking to string values
			config[key] = redactValue(fullKey, v)
		default:
			// For other types (numbers, booleans, etc.), convert to string and check for masking
			if str := fmt.Sprint(v); str != "" {
				masked := redactValue(fullKey, str)
				if masked != str {
					// Only replace if masking was applied
					config[key] = masked
				}
			}
		}
	}
}

// getConfig 返回当前配置（已脱敏）。
// 先序列化再反序列化成 map，以复用结构体上的 JSON 标签得到与配置文件一致的字段名。
func getConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Marshal the actual configuration struct to preserve original field names
	configBytes, err := json.Marshal(*conf.Server)
	if err != nil {
		log.Error(ctx, "Error marshaling config", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Unmarshal back to map to get the structure with proper field names
	var configMap map[string]interface{}
	err = json.Unmarshal(configBytes, &configMap)
	if err != nil {
		log.Error(ctx, "Error unmarshaling config to map", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Apply sensitive field masking
	applySensitiveFieldMasking(ctx, configMap, "")

	resp := configResponse{
		ID:         "config",
		ConfigFile: conf.Server.ConfigFile,
		Config:     configMap,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Error(ctx, "Error encoding config response", err)
	}
}
