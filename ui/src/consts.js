export const REST_URL = '/api'

export const INSIGHTS_DOC_URL =
  'https://navidrome.org/docs/getting-started/insights'

export const M3U_MIME_TYPE = 'audio/x-mpegurl'

export const AUTO_THEME_ID = 'AUTO_THEME_ID'

// 拖拽类型，用于把歌曲/专辑/艺人拖入播放列表。
export const DraggableTypes = {
  SONG: 'song',
  ALBUM: 'album',
  DISC: 'disc',
  ARTIST: 'artist',
  ALL: [],
}

DraggableTypes.ALL.push(
  DraggableTypes.SONG,
  DraggableTypes.ALBUM,
  DraggableTypes.DISC,
  DraggableTypes.ARTIST,
)

export const DEFAULT_SHARE_BITRATE = 128

// 分享链接可选的码率档位。
export const BITRATE_CHOICES = [
  32, 48, 64, 80, 96, 112, 128, 160, 192, 256, 320,
].map((b) => ({ id: b, name: b.toString() }))
