package plugins

import (
	"fmt"
	"strings"

	"github.com/navidrome/navidrome/plugins/schema"
)

// Maximum number of HTTP redirects allowed for plugin requests
// httpMaxRedirects 限制重定向次数，防止插件被重定向链拖住或绕过限制。
const httpMaxRedirects = 5

// HTTPPermissions represents granular HTTP access permissions for plugins
// httpPermissions 描述插件可访问的 URL 模式及其允许的方法。
type httpPermissions struct {
	*networkPermissionsBase
	AllowedUrls map[string][]string `json:"allowedUrls"`
	matcher     *urlMatcher
}

// parseHTTPPermissions extracts HTTP permissions from the schema
// parseHTTPPermissions 解析 HTTP 权限声明。
// 白名单不可为空：默认拒绝一切访问，插件必须显式列出所需地址。
func parseHTTPPermissions(permData *schema.PluginManifestPermissionsHttp) (*httpPermissions, error) {
	base := &networkPermissionsBase{
		AllowLocalNetwork: permData.AllowLocalNetwork,
	}

	if len(permData.AllowedUrls) == 0 {
		return nil, fmt.Errorf("allowedUrls must contain at least one URL pattern")
	}

	allowedUrls := make(map[string][]string)
	for urlPattern, methodEnums := range permData.AllowedUrls {
		methods := make([]string, len(methodEnums))
		for i, methodEnum := range methodEnums {
			methods[i] = string(methodEnum)
		}
		allowedUrls[urlPattern] = methods
	}

	return &httpPermissions{
		networkPermissionsBase: base,
		AllowedUrls:            allowedUrls,
		matcher:                newURLMatcher(),
	}, nil
}

// IsRequestAllowed checks if a specific network request is allowed by the permissions
//
// IsRequestAllowed 校验请求是否被允许。
// 先做本地网络检查（防 SSRF 探测内网），再匹配白名单。
// 精确匹配优先于通配匹配，使更具体的规则能覆盖宽泛规则。
func (p *httpPermissions) IsRequestAllowed(requestURL, operation string) error {
	if _, err := checkURLPolicy(requestURL, p.AllowLocalNetwork); err != nil {
		return err
	}

	// allowedUrls is now required - no fallback to allow all URLs
	if p.AllowedUrls == nil || len(p.AllowedUrls) == 0 {
		return fmt.Errorf("no allowed URLs configured for plugin")
	}

	matcher := newURLMatcher()

	// Check URL patterns and operations
	// First try exact matches, then wildcard matches
	operation = strings.ToUpper(operation)

	// Phase 1: Check for exact matches first
	for urlPattern, allowedOperations := range p.AllowedUrls {
		if !strings.Contains(urlPattern, "*") && matcher.MatchesURLPattern(requestURL, urlPattern) {
			// Check if operation is allowed
			for _, allowedOperation := range allowedOperations {
				if allowedOperation == "*" || allowedOperation == operation {
					return nil
				}
			}
			return fmt.Errorf("operation %s not allowed for URL pattern %s", operation, urlPattern)
		}
	}

	// Phase 2: Check wildcard patterns
	for urlPattern, allowedOperations := range p.AllowedUrls {
		if strings.Contains(urlPattern, "*") && matcher.MatchesURLPattern(requestURL, urlPattern) {
			// Check if operation is allowed
			for _, allowedOperation := range allowedOperations {
				if allowedOperation == "*" || allowedOperation == operation {
					return nil
				}
			}
			return fmt.Errorf("operation %s not allowed for URL pattern %s", operation, urlPattern)
		}
	}

	return fmt.Errorf("URL %s does not match any allowed URL patterns", requestURL)
}
