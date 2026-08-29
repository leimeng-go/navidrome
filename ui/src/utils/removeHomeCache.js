// removeHomeCache 清除 Service Worker 缓存的 index.html。
// 该页面内联了服务端注入的用户配置，登录状态变化后必须重新拉取，
// 否则会读到上一个用户的配置。
export const removeHomeCache = async () => {
  try {
    const workboxKey = (await caches.keys()).find((key) =>
      key.startsWith('workbox-precache'),
    )
    if (!workboxKey) return

    const workboxCache = await caches.open(workboxKey)
    const indexKey = (await workboxCache.keys()).find((key) =>
      key.url.includes('app/index.html'),
    )

    if (indexKey) {
      await workboxCache.delete(indexKey)
    }
  } catch (e) {
    // eslint-disable-next-line no-console
    console.error('error reading cache', e)
  }
}
