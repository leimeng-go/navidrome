import { baseUrl } from '../utils'
import { httpClient } from '../dataProvider'

// url 拼装 Subsonic API 请求地址。
// 用 salt+token 的方式认证（不传明文密码），凭据在登录时由服务端下发。
// options.ts 为 true 时附加时间戳，用于绕过浏览器缓存。
const url = (command, id, options) => {
  const username = localStorage.getItem('username')
  const token = localStorage.getItem('subsonic-token')
  const salt = localStorage.getItem('subsonic-salt')
  if (!username || !token || !salt) {
    return ''
  }

  const params = new URLSearchParams()
  params.append('u', username)
  params.append('t', token)
  params.append('s', salt)
  params.append('f', 'json')
  params.append('v', '1.8.0')
  params.append('c', 'NavidromeUI')
  id && params.append('id', id)
  if (options) {
    if (options.ts) {
      options['_'] = new Date().getTime()
      delete options.ts
    }
    Object.keys(options).forEach((k) => {
      const value = options[k]
      // Handle array parameters by appending each value separately
      if (Array.isArray(value)) {
        value.forEach((v) => params.append(k, v))
      } else {
        params.append(k, value)
      }
    })
  }
  return `/rest/${command}?${params.toString()}`
}

const ping = () => httpClient(url('ping'))

// scrobble 上报播放记录。
// submission=true 表示播放完成（计入播放次数并转发给 Last.fm 等），
// false 表示「正在播放」心跳，只更新当前播放状态。
const scrobble = (id, time, submission = true, position = null) =>
  httpClient(
    url('scrobble', id, {
      ...(submission && time && { time }),
      submission,
      ...(!submission && position !== null && { position }),
    }),
  )

const nowPlaying = (id, position = null) => scrobble(id, null, false, position)

const star = (id) => httpClient(url('star', id))

const unstar = (id) => httpClient(url('unstar', id))

const setRating = (id, rating) => httpClient(url('setRating', id, { rating }))

const download = (id, format = 'raw', bitrate = '0') =>
  (window.location.href = baseUrl(url('download', id, { format, bitrate })))

const startScan = (options) => httpClient(url('startScan', null, options))

const getScanStatus = () => httpClient(url('getScanStatus'))

const getNowPlaying = () => httpClient(url('getNowPlaying'))

const getAvatarUrl = (username, size) =>
  baseUrl(
    url('getAvatar', null, {
      username,
      ...(size && { size }),
    }),
  )

// getCoverArtUrl 根据记录类型推断封面 ID 前缀。
// 服务端用 mf-/al-/pl-/ar- 区分歌曲、专辑、歌单与艺人封面。
// 带上 updatedAt 作为缓存击穿参数，使封面更新后能立即生效。
const getCoverArtUrl = (record, size, square) => {
  const options = {
    ...(record.updatedAt && { _: record.updatedAt }),
    ...(size && { size }),
    ...(square && { square }),
  }

  // TODO Move this logic to server
  if (record.album) {
    return baseUrl(url('getCoverArt', 'mf-' + record.id, options))
  } else if (record.albumArtist) {
    return baseUrl(url('getCoverArt', 'al-' + record.id, options))
  } else if (record.sync !== undefined) {
    // This is a playlist
    return baseUrl(url('getCoverArt', 'pl-' + record.id, options))
  } else {
    return baseUrl(url('getCoverArt', 'ar-' + record.id, options))
  }
}

const getArtistInfo = (id) => {
  return httpClient(url('getArtistInfo', id))
}

const getAlbumInfo = (id) => {
  return httpClient(url('getAlbumInfo', id))
}

const getSimilarSongs2 = (id, count = 100) => {
  return httpClient(url('getSimilarSongs2', id, { count }))
}

const getTopSongs = (artist, count = 50) => {
  return httpClient(url('getTopSongs', null, { artist, count }))
}

const streamUrl = (id, options) => {
  return baseUrl(
    url('stream', id, {
      ts: true,
      ...options,
    }),
  )
}

export default {
  url,
  ping,
  scrobble,
  nowPlaying,
  download,
  star,
  unstar,
  setRating,
  startScan,
  getScanStatus,
  getNowPlaying,
  getCoverArtUrl,
  getAvatarUrl,
  streamUrl,
  getAlbumInfo,
  getArtistInfo,
  getTopSongs,
  getSimilarSongs2,
}
