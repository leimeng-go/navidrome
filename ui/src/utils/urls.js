import config from '../config'

// baseUrl 拼接部署路径前缀，支持部署在子目录下。
export const baseUrl = (path) => {
  const base = config.baseURL || ''
  const parts = [base]
  parts.push(path.replace(/^\//, ''))
  return parts.join('/')
}

// shareUrl 生成分享链接。
// 配置了 shareURL 时用它（通常是对外可访问的公网地址），
// 否则退回当前站点地址。
export const shareUrl = (path) => {
  if (config.shareURL !== '') {
    const base = config.shareURL || ''
    const parts = [base]
    parts.push(path.replace(/^\//, ''))
    return parts.join('/')
  }
  return baseUrl(path)
}

export const sharePlayerUrl = (id) => {
  const url = new URL(
    shareUrl(config.publicBaseUrl + '/' + id),
    window.location.href,
  )
  return url.href
}

export const shareStreamUrl = (id) => {
  return shareUrl(config.publicBaseUrl + '/s/' + id)
}

export const shareDownloadUrl = (id) => {
  return shareUrl(config.publicBaseUrl + '/d/' + id)
}

export const shareCoverUrl = (id, square) => {
  return shareUrl(
    config.publicBaseUrl +
      '/img/' +
      id +
      '?size=300' +
      (square ? '&square=true' : ''),
  )
}

export const docsUrl = (path) => `https://www.navidrome.org${path}`
