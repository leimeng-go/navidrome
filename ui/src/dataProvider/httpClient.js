import { fetchUtils } from 'react-admin'
import { v4 as uuidv4 } from 'uuid'
import { baseUrl } from '../utils'
import config from '../config'
import { jwtDecode } from 'jwt-decode'
import { removeHomeCache } from '../utils/removeHomeCache'

// 使用自定义头而非标准 Authorization：
// 避免与反向代理注入的认证头冲突。
const customAuthorizationHeader = 'X-ND-Authorization'
const clientUniqueIdHeader = 'X-ND-Client-Unique-Id'
// 每个浏览器标签页一个唯一 ID，服务端据此区分播放会话。
const clientUniqueId = uuidv4()

// httpClient 统一注入认证头，并处理服务端下发的续期 token，
// 使长时间使用的会话不会因 JWT 过期而被登出。
const httpClient = (url, options = {}) => {
  url = baseUrl(url)
  if (!options.headers) {
    options.headers = new Headers({ Accept: 'application/json' })
  }
  options.headers.set(clientUniqueIdHeader, clientUniqueId)
  const token = localStorage.getItem('token')
  if (token) {
    options.headers.set(customAuthorizationHeader, `Bearer ${token}`)
  }
  return fetchUtils.fetchJson(url, options).then((response) => {
    const token = response.headers.get(customAuthorizationHeader)
    if (token) {
      const decoded = jwtDecode(token)
      localStorage.setItem('token', token)
      localStorage.setItem('userId', decoded.uid)
      // Avoid going to create admin dialog after logout/login without a refresh
      config.firstTime = false
      removeHomeCache()
    }
    return response
  })
}

export default httpClient
