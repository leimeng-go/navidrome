package plugins

import (
	"context"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/plugins/host/config"
)

// configServiceImpl 向插件暴露其专属配置。
// 只返回以该插件 ID 为键的那一段，插件无法读取全局配置或他人配置。
type configServiceImpl struct {
	pluginID string
}

func (c *configServiceImpl) GetPluginConfig(ctx context.Context, req *config.GetPluginConfigRequest) (*config.GetPluginConfigResponse, error) {
	cfg, ok := conf.Server.PluginConfig[c.pluginID]
	if !ok {
		cfg = map[string]string{}
	}
	return &config.GetPluginConfigResponse{
		Config: cfg,
	}, nil
}
