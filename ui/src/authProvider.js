import { jwtDecode } from 'jwt-decode'
import { baseUrl } from './utils'
import config from './config'
import { removeHomeCache } from './utils/removeHomeCache'

// config sent from server may contain authentication info, for example when the user is authenticated
// by a reverse proxy request header
if (config.auth) {
  try {
    storeAuthenticationInfo(config.auth)
  } catch (e) {
    // eslint-disable-next-line no-console
    console.log(e)
  }
}

// storeAuthenticationInfo 把认证信息写入 localStorage。
// 同时保存 subsonic 的 salt 与 token：内置播放器走 Subsonic API 取流，
// 该套接口用的是独立于 JWT 的认证方式。
function storeAuthenticationInfo(authInfo) {
  authInfo.token && localStorage.setItem('token', authInfo.token)
  localStorage.setItem('userId', authInfo.id)
  localStorage.setItem('name', authInfo.name)
  localStorage.setItem('username', authInfo.username)
  authInfo.avatar && localStorage.setItem('avatar', authInfo.avatar)
  localStorage.setItem('role', authInfo.isAdmin ? 'admin' : 'regular')
  localStorage.setItem('subsonic-salt', authInfo.subsonicSalt)
  localStorage.setItem('subsonic-token', authInfo.subsonicToken)
  localStorage.setItem('is-authenticated', 'true')
}

// authProvider 是 react-admin 的认证适配器。
const authProvider = {
  login: ({ username, password }) => {
    let url = baseUrl('/auth/login')
    if (config.firstTime) {
      url = baseUrl('/auth/createAdmin')
    }
    const request = new Request(url, {
      method: 'POST',
      body: JSON.stringify({ username, password }),
      headers: new Headers({ 'Content-Type': 'application/json' }),
    })
    return fetch(request)
      .then((response) => {
        if (response.status < 200 || response.status >= 300) {
          throw new Error(response.statusText)
        }
        return response.json()
      })
      .then((response) => {
        jwtDecode(response.token) // Validate token
        storeAuthenticationInfo(response)
        // Avoid "going to create admin" dialog after logout/login without a refresh
        config.firstTime = false
        removeHomeCache()
        return response
      })
      .catch((error) => {
        if (
          error.message === 'Failed to fetch' ||
          error.stack === 'TypeError: Failed to fetch'
        ) {
          throw new Error('errors.network_error')
        }

        throw new Error(error)
      })
  },

  logout: () => {
    removeItems()
    return Promise.resolve()
  },

  checkAuth: () =>
    localStorage.getItem('is-authenticated')
      ? Promise.resolve()
      : Promise.reject(),

  checkError: ({ status }) => {
    if (status === 401) {
      removeItems()
      return Promise.reject()
    }
    return Promise.resolve()
  },

  getPermissions: () => {
    const role = localStorage.getItem('role')
    return role ? Promise.resolve(role) : Promise.reject()
  },

  getIdentity: () => {
    return Promise.resolve({
      id: localStorage.getItem('username'),
      fullName: localStorage.getItem('name'),
      avatar: localStorage.getItem('avatar'),
    })
  },
}

// removeItems 清除全部认证痕迹，登出与鉴权失效时调用。
const removeItems = () => {
  localStorage.removeItem('token')
  localStorage.removeItem('userId')
  localStorage.removeItem('name')
  localStorage.removeItem('username')
  localStorage.removeItem('avatar')
  localStorage.removeItem('role')
  localStorage.removeItem('subsonic-salt')
  localStorage.removeItem('subsonic-token')
  localStorage.removeItem('is-authenticated')
}

export default authProvider
