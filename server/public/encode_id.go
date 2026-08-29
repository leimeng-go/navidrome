package public

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strconv"

	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/auth"
	"github.com/navidrome/navidrome/model"
	. "github.com/navidrome/navidrome/utils/gg"
)

// ImageURL 生成公开图片链接，图片 ID 以签名令牌形式携带。
func ImageURL(r *http.Request, artID model.ArtworkID, size int) string {
	token := encodeArtworkID(artID)
	uri := path.Join(consts.URLPathPublicImages, token)
	params := url.Values{}
	if size > 0 {
		params.Add("size", strconv.Itoa(size))
	}
	return publicURL(r, uri, params)
}

// encodeArtworkID 把封面 ID 编码成签名令牌。
// 直接暴露内部 ID 会让任何人可遍历全库封面，签名令牌可杜绝此类枚举。
func encodeArtworkID(artID model.ArtworkID) string {
	token, _ := auth.CreatePublicToken(map[string]any{"id": artID.String()})
	return token
}

// decodeArtworkID 校验并解出封面 ID。
// 若解析失败，再按媒体文件封面重试一次——
// 单曲分享的令牌里存的是媒体文件 ID，不带类型前缀。
func decodeArtworkID(tokenString string) (model.ArtworkID, error) {
	token, err := auth.TokenAuth.Decode(tokenString)
	if err != nil {
		return model.ArtworkID{}, err
	}
	if token == nil {
		return model.ArtworkID{}, errors.New("unauthorized")
	}
	err = jwt.Validate(token, jwt.WithRequiredClaim("id"))
	if err != nil {
		return model.ArtworkID{}, err
	}
	claims, err := token.AsMap(context.Background())
	if err != nil {
		return model.ArtworkID{}, err
	}
	id, ok := claims["id"].(string)
	if !ok {
		return model.ArtworkID{}, errors.New("invalid id type")
	}
	artID, err := model.ParseArtworkID(id)
	if err == nil {
		return artID, nil
	}
	// Try to default to mediafile artworkId (if used with a mediafileShare token)
	return model.ParseArtworkID("mf-" + id)
}

// encodeMediafileShare 为分享中的单个媒体文件签发令牌。
// 转码格式与码率一并写入声明，使播放参数不可被客户端篡改。
// 令牌有效期跟随分享本身的过期时间。
func encodeMediafileShare(s model.Share, id string) string {
	claims := map[string]any{"id": id}
	if s.Format != "" {
		claims["f"] = s.Format
	}
	if s.MaxBitRate != 0 {
		claims["b"] = s.MaxBitRate
	}
	token, _ := auth.CreateExpiringPublicToken(V(s.ExpiresAt), claims)
	return token
}
