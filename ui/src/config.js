// These defaults are only used in development mode. When bundled in the app,
// the __APP_CONFIG__ object is dynamically filled by the ServeIndex function,
// in the /server/app/serve_index.go
const defaultConfig = {
  version: 'dev',
  firstTime: false,
  baseURL: '',
  variousArtistsId: '63sqASlAfjbGMuLP4JhnZU', // See consts.VariousArtistsID in consts.go
  // Login backgrounds from https://unsplash.com/collections/1065384/music-wallpapers
  loginBackgroundURL: 'https://source.unsplash.com/collection/1065384/1600x900',
  maxSidebarPlaylists: 100,
  enableTranscodingConfig: true,
  enableDownloads: true,
  enableFavourites: true,
  losslessFormats: 'FLAC,WAV,ALAC,DSF',
  welcomeMessage: '',
  gaTrackingId: '',
  devActivityPanel: true,
  enableStarRating: true,
  defaultTheme: 'Dark',
  defaultLanguage: '',
  defaultUIVolume: 100,
  enableUserEditing: true,
  enableSharing: true,
  shareURL: '',
  defaultDownloadableShare: true,
  devSidebarPlaylists: true,
  lastFMEnabled: true,
  listenBrainzEnabled: true,
  enableExternalServices: true,
  enableCoverAnimation: true,
  enableNowPlaying: true,
  devShowArtistPage: true,
  devUIShowConfig: true,
  devNewEventStream: false,
  enableReplayGain: true,
  defaultDownsamplingFormat: 'opus',
  publicBaseUrl: '/share',
  separator: '/',
  enableInspect: true,
}

// 优先使用服务端注入的配置；解析失败（如开发模式下未注入）时退回默认值。
let config

try {
  const appConfig = JSON.parse(window.__APP_CONFIG__)
  config = {
    ...defaultConfig,
    ...appConfig,
  }
} catch (e) {
  config = defaultConfig
}

// shareInfo 仅在分享页面由服务端注入，其余页面为 null。
export let shareInfo

try {
  shareInfo = JSON.parse(window.__SHARE_INFO__)
} catch (e) {
  shareInfo = null
}

export default config
