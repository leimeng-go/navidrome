export const formatBytes = (bytes, decimals = 2) => {
  if (bytes === 0) return '0 Bytes'

  const k = 1024
  const dm = decimals < 0 ? 0 : decimals
  const sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB', 'PB', 'EB', 'ZB', 'YB']

  const i = Math.floor(Math.log(bytes) / Math.log(k))

  return parseFloat((bytes / Math.pow(k, i)).toFixed(dm)) + ' ' + sizes[i]
}

// formatDuration 格式化为 [d:]hh:mm:ss，前导的零位分段会被省略。
export const formatDuration = (d) => {
  d = Math.round(d)
  const days = Math.floor(d / 86400)
  const hours = Math.floor(d / 3600) % 24
  const minutes = Math.floor(d / 60) % 60
  const seconds = Math.floor(d % 60)
  const f = [hours, minutes, seconds]
    .map((v) => v.toString())
    .map((v) => (v.length !== 2 ? '0' + v : v))
    .filter((v, i) => v !== '00' || i > 0 || days > 0)
    .join(':')

  return `${days > 0 ? days + ':' : ''}${f}`
}

// formatDuration2 格式化为 "1d 2h 3m" 形式，最多显示三级，用于统计类展示。
export const formatDuration2 = (totalSeconds) => {
  if (totalSeconds == null || totalSeconds < 0) {
    return '0s'
  }
  const days = Math.floor(totalSeconds / 86400)
  const hours = Math.floor((totalSeconds % 86400) / 3600)
  const minutes = Math.floor((totalSeconds % 3600) / 60)
  const seconds = Math.floor(totalSeconds % 60)

  const parts = []

  if (days > 0) {
    // When days are present, show only d h m (3 levels max)
    parts.push(`${days}d`)
    if (hours > 0) {
      parts.push(`${hours}h`)
    }
    if (minutes > 0) {
      parts.push(`${minutes}m`)
    }
  } else {
    // When no days, show h m s (3 levels max)
    if (hours > 0) {
      parts.push(`${hours}h`)
    }
    if (minutes > 0) {
      parts.push(`${minutes}m`)
    }
    if (seconds > 0 || parts.length === 0) {
      parts.push(`${seconds}s`)
    }
  }

  return parts.join(' ')
}

// formatShortDuration 格式化纳秒级耗时（Go 的 time.Duration 序列化为纳秒）。
export const formatShortDuration = (ns) => {
  // Convert nanoseconds to seconds
  const seconds = ns / 1e9
  if (seconds < 1.0) {
    return '<1s'
  }

  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const secs = Math.floor(seconds % 60)

  if (hours > 0) {
    return `${hours}h${minutes}m`
  }
  if (minutes > 0) {
    return `${minutes}m${secs}s`
  }
  return `${secs}s`
}

// formatFullDate 按日期字符串的精度自适应显示。
// 音乐标签中的日期常只精确到年或年月（如 "1969"、"1969-08"），
// 按短横线数量决定展示到年、月还是日；格式无法识别时返回空串。
export const formatFullDate = (date, locale) => {
  const dashes = date.split('-').length - 1
  let options = {
    year: 'numeric',
    timeZone: 'UTC',
    ...(dashes > 0 && { month: 'short' }),
    ...(dashes > 1 && { day: 'numeric' }),
  }
  if (dashes > 2 || (dashes === 0 && date.length > 4)) {
    return ''
  }
  return new Date(date).toLocaleDateString(locale, options)
}

export const formatNumber = (value, locale) => {
  if (value === null || value === undefined) return '0'
  return value.toLocaleString(locale)
}
