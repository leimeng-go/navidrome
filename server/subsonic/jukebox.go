package subsonic

import (
	"net/http"
	"strconv"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core/playback"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/server/subsonic/responses"
	"github.com/navidrome/navidrome/utils/req"
	"github.com/navidrome/navidrome/utils/slice"
)

// Jukebox 支持的控制动作。
const (
	ActionGet     = "get"
	ActionStatus  = "status"
	ActionSet     = "set"
	ActionStart   = "start"
	ActionStop    = "stop"
	ActionSkip    = "skip"
	ActionAdd     = "add"
	ActionClear   = "clear"
	ActionRemove  = "remove"
	ActionShuffle = "shuffle"
	ActionSetGain = "setGain"
)

// JukeboxControl 控制服务端本地播放（Jukebox）。
//
// 该功能让服务器直接驱动本机声卡，属于高权限操作，
// 故需显式启用，并可配置为仅管理员可用。
// 每个用户对应一个独立设备，动作通过 action 参数分发。
func (api *Router) JukeboxControl(r *http.Request) (*responses.Subsonic, error) {
	ctx := r.Context()
	user := getUser(ctx)
	p := req.Params(r)

	if !conf.Server.Jukebox.Enabled {
		return nil, newError(responses.ErrorGeneric, "Jukebox is disabled")
	}

	if conf.Server.Jukebox.AdminOnly && !user.IsAdmin {
		return nil, newError(responses.ErrorAuthorizationFail, "Jukebox is admin only")
	}

	actionString, err := p.String("action")
	if err != nil {
		return nil, err
	}

	pb, err := api.playback.GetDeviceForUser(user.UserName)
	if err != nil {
		return nil, err
	}
	log.Info(ctx, "JukeboxControl request received", "action", actionString)

	switch actionString {
	case ActionGet:
		mediafiles, status, err := pb.Get(ctx)
		if err != nil {
			return nil, err
		}

		playlist := responses.JukeboxPlaylist{
			JukeboxStatus: *deviceStatusToJukeboxStatus(status),
			Entry:         slice.MapWithArg(mediafiles, ctx, childFromMediaFile),
		}

		response := newResponse()
		response.JukeboxPlaylist = &playlist
		return response, nil
	case ActionStatus:
		return createResponse(pb.Status(ctx))
	case ActionSet:
		ids, _ := p.Strings("id")
		return createResponse(pb.Set(ctx, ids))
	case ActionStart:
		return createResponse(pb.Start(ctx))
	case ActionStop:
		return createResponse(pb.Stop(ctx))
	case ActionSkip:
		index, err := p.Int("index")
		if err != nil {
			return nil, newError(responses.ErrorMissingParameter, "missing parameter index, err: %s", err)
		}
		offset := p.IntOr("offset", 0)
		return createResponse(pb.Skip(ctx, index, offset))
	case ActionAdd:
		ids, _ := p.Strings("id")
		return createResponse(pb.Add(ctx, ids))
	case ActionClear:
		return createResponse(pb.Clear(ctx))
	case ActionRemove:
		index, err := p.Int("index")
		if err != nil {
			return nil, err
		}

		return createResponse(pb.Remove(ctx, index))
	case ActionShuffle:
		return createResponse(pb.Shuffle(ctx))
	case ActionSetGain:
		gainStr, err := p.String("gain")
		if err != nil {
			return nil, newError(responses.ErrorMissingParameter, "missing parameter gain, err: %s", err)
		}

		gain, err := strconv.ParseFloat(gainStr, 32)
		if err != nil {
			return nil, newError(responses.ErrorMissingParameter, "error parsing gain float value, err: %s", err)
		}

		return createResponse(pb.SetGain(ctx, float32(gain)))
	default:
		return nil, newError(responses.ErrorMissingParameter, "Unknown action: %s", actionString)
	}
}

// createResponse is to shorten the case-switch in the JukeboxController
func createResponse(status playback.DeviceStatus, err error) (*responses.Subsonic, error) {
	if err != nil {
		return nil, err
	}
	return statusResponse(status), nil
}

// statusResponse 构造仅含状态的响应。
func statusResponse(status playback.DeviceStatus) *responses.Subsonic {
	response := newResponse()
	response.JukeboxStatus = deviceStatusToJukeboxStatus(status)
	return response
}

// deviceStatusToJukeboxStatus 转换播放设备状态为协议结构。
func deviceStatusToJukeboxStatus(status playback.DeviceStatus) *responses.JukeboxStatus {
	return &responses.JukeboxStatus{
		CurrentIndex: int32(status.CurrentIndex),
		Playing:      status.Playing,
		Gain:         status.Gain,
		Position:     int32(status.Position),
	}
}
