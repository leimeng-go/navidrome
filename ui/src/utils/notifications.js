// sendNotification 发送切歌桌面通知，静音以免与音乐播放冲突。
export const sendNotification = (title, body = '', image = '') => {
  checkForNotificationPermission()
  new Notification(title, {
    body: body,
    icon: image,
    silent: true,
  })
}

const checkForNotificationPermission = () => {
  return 'Notification' in window && Notification.permission === 'granted'
}
