package plugins

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// NetworkPermissionsBase contains common functionality for network-based permissions
// networkPermissionsBase 是 HTTP 与 WebSocket 权限的公共部分。
type networkPermissionsBase struct {
	Reason            string `json:"reason"`
	AllowLocalNetwork bool   `json:"allowLocalNetwork,omitempty"`
}

// URLMatcher provides URL pattern matching functionality
// urlMatcher 负责 URL 与通配模式的匹配。
type urlMatcher struct{}

// newURLMatcher creates a new URL matcher instance
func newURLMatcher() *urlMatcher {
	return &urlMatcher{}
}

// checkURLPolicy performs common checks for a URL against network policies.
// checkURLPolicy 做与具体协议无关的通用校验。
func checkURLPolicy(requestURL string, allowLocalNetwork bool) (*url.URL, error) {
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	// Check local network restrictions
	if !allowLocalNetwork {
		if err := checkLocalNetwork(parsedURL); err != nil {
			return nil, err
		}
	}
	return parsedURL, nil
}

// MatchesURLPattern checks if a URL matches a given pattern
//
// MatchesURLPattern 判断 URL 是否匹配模式，按 scheme/host/path 分段比对。
// 模式不是合法 URL 时退化为整串正则匹配。
// 仅含域名通配（无路径）的模式视为放行该域名下的任意路径。
func (m *urlMatcher) MatchesURLPattern(requestURL, pattern string) bool {
	// Handle wildcard pattern
	if pattern == "*" {
		return true
	}

	// Parse both URLs to handle path matching correctly
	reqURL, err := url.Parse(requestURL)
	if err != nil {
		return false
	}

	patternURL, err := url.Parse(pattern)
	if err != nil {
		// If pattern is not a valid URL, treat it as a simple string pattern
		regexPattern := m.urlPatternToRegex(pattern)
		matched, err := regexp.MatchString(regexPattern, requestURL)
		if err != nil {
			return false
		}
		return matched
	}

	// Match scheme
	if patternURL.Scheme != "" && patternURL.Scheme != reqURL.Scheme {
		return false
	}

	// Match host with wildcard support
	if !m.matchesHost(reqURL.Host, patternURL.Host) {
		return false
	}

	// Match path with wildcard support
	// Special case: if pattern URL has empty path and contains wildcards, allow any path (domain-only wildcard matching)
	if (patternURL.Path == "" || patternURL.Path == "/") && strings.Contains(pattern, "*") {
		// This is a domain-only wildcard pattern, allow any path
		return true
	}
	if !m.matchesPath(reqURL.Path, patternURL.Path) {
		return false
	}

	return true
}

// urlPatternToRegex converts a URL pattern with wildcards to a regex pattern
// urlPatternToRegex 把通配模式转成锚定的正则，其余字符全部转义以免被当作正则元字符。
func (m *urlMatcher) urlPatternToRegex(pattern string) string {
	// Escape special regex characters except *
	escaped := regexp.QuoteMeta(pattern)

	// Replace escaped \* with regex pattern for wildcard matching
	// For subdomain: *.example.com -> [^.]*\.example\.com
	// For path: /api/* -> /api/.*
	escaped = strings.ReplaceAll(escaped, "\\*", ".*")

	// Anchor the pattern to match the full URL
	return "^" + escaped + "$"
}

// matchesHost checks if a host matches a pattern with wildcard support
// matchesHost 匹配主机名。
// 通配符同时按 IP 段与域名两种语义各试一次，因为 * 在
// 192.168.*.* 与 *.example.com 中的含义不同。
func (m *urlMatcher) matchesHost(host, pattern string) bool {
	if pattern == "" {
		return true
	}

	if pattern == "*" {
		return true
	}

	// Handle wildcard patterns anywhere in the host
	if strings.Contains(pattern, "*") {
		patterns := []string{
			strings.ReplaceAll(regexp.QuoteMeta(pattern), "\\*", "[0-9.]+"), // IP pattern
			strings.ReplaceAll(regexp.QuoteMeta(pattern), "\\*", "[^.]*"),   // Domain pattern
		}

		for _, regexPattern := range patterns {
			fullPattern := "^" + regexPattern + "$"
			if matched, err := regexp.MatchString(fullPattern, host); err == nil && matched {
				return true
			}
		}
		return false
	}

	return host == pattern
}

// matchesPath checks if a path matches a pattern with wildcard support
// matchesPath 匹配路径，/* 结尾表示前缀匹配。
func (m *urlMatcher) matchesPath(path, pattern string) bool {
	// Normalize empty paths to "/"
	if path == "" {
		path = "/"
	}
	if pattern == "" {
		pattern = "/"
	}

	if pattern == "*" {
		return true
	}

	// Handle wildcard paths
	if strings.HasSuffix(pattern, "/*") {
		prefix := pattern[:len(pattern)-2] // Remove "/*"
		if prefix == "" {
			prefix = "/"
		}
		return strings.HasPrefix(path, prefix)
	}

	return path == pattern
}

// CheckLocalNetwork checks if the URL is accessing local network resources
// checkLocalNetwork 阻止访问本机与内网地址，这是防 SSRF 的基本措施：
// 否则插件可借服务器身份探测内网服务或读取云元数据接口。
func checkLocalNetwork(parsedURL *url.URL) error {
	host := parsedURL.Hostname()

	// Check for localhost variants
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return fmt.Errorf("requests to localhost are not allowed")
	}

	// Try to parse as IP address
	ip := net.ParseIP(host)
	if ip != nil && isPrivateIP(ip) {
		return fmt.Errorf("requests to private IP addresses are not allowed")
	}

	return nil
}

// IsPrivateIP checks if an IP is loopback, private, or link-local (IPv4/IPv6).
// isPrivateIP 判断是否为回环、私有或链路本地地址。
// 链路本地需单独判断：云平台的元数据服务常位于 169.254.169.254。
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	// IPv4 link-local: 169.254.0.0/16
	if ip4 := ip.To4(); ip4 != nil {
		return ip4[0] == 169 && ip4[1] == 254
	}
	// IPv6 link-local: fe80::/10
	if ip16 := ip.To16(); ip16 != nil && ip.To4() == nil {
		return ip16[0] == 0xfe && (ip16[1]&0xc0) == 0x80
	}
	return false
}
