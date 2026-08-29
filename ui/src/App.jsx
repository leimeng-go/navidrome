import ReactGA from 'react-ga'
import { Provider } from 'react-redux'
import { createHashHistory } from 'history'
import { Admin as RAAdmin, Resource } from 'react-admin'
import { HotKeys } from 'react-hotkeys'
import dataProvider from './dataProvider'
import authProvider from './authProvider'
import { Layout, Login, Logout } from './layout'
import transcoding from './transcoding'
import player from './player'
import user from './user'
import song from './song'
import album from './album'
import artist from './artist'
import playlist from './playlist'
import radio from './radio'
import share from './share'
import library from './library'
import { Player } from './audioplayer'
import customRoutes from './routes'
import {
  libraryReducer,
  themeReducer,
  addToPlaylistDialogReducer,
  expandInfoDialogReducer,
  listenBrainzTokenDialogReducer,
  saveQueueDialogReducer,
  playerReducer,
  albumViewReducer,
  activityReducer,
  settingsReducer,
  replayGainReducer,
  downloadMenuDialogReducer,
  shareDialogReducer,
} from './reducers'
import createAdminStore from './store/createAdminStore'
import { i18nProvider } from './i18n'
import config, { shareInfo } from './config'
import { keyMap } from './hotkeys'
import useChangeThemeColor from './useChangeThemeColor'
import SharePlayer from './share/SharePlayer'
import { HTML5Backend } from 'react-dnd-html5-backend'
import { DndProvider } from 'react-dnd'
import missing from './missing/index.js'

// 使用 hash 路由：Navidrome 可能被部署在任意子路径下，
// hash 模式无需服务端为前端路由做额外的 rewrite 配置。
const history = createHashHistory()

if (config.gaTrackingId) {
  ReactGA.initialize(config.gaTrackingId)
  history.listen((location) => {
    ReactGA.pageview(location.pathname)
  })
  ReactGA.pageview(window.location.pathname)
}

const adminStore = createAdminStore({
  authProvider,
  dataProvider,
  history,
  customReducers: {
    library: libraryReducer,
    player: playerReducer,
    albumView: albumViewReducer,
    theme: themeReducer,
    addToPlaylistDialog: addToPlaylistDialogReducer,
    downloadMenuDialog: downloadMenuDialogReducer,
    expandInfoDialog: expandInfoDialogReducer,
    listenBrainzTokenDialog: listenBrainzTokenDialogReducer,
    saveQueueDialog: saveQueueDialogReducer,
    shareDialog: shareDialogReducer,
    activity: activityReducer,
    settings: settingsReducer,
    replayGain: replayGainReducer,
  },
})

const App = () => (
  <Provider store={adminStore}>
    <Admin />
  </Provider>
)

// Admin 按权限与配置开关动态注册资源。
// 非管理员仍注册空的 transcoding 资源：
// 播放器需要引用它做数据关联，缺失会导致 react-admin 报错。
const Admin = (props) => {
  useChangeThemeColor()
  /* eslint-disable react/jsx-key */
  return (
    <RAAdmin
      disableTelemetry
      dataProvider={dataProvider}
      authProvider={authProvider}
      i18nProvider={i18nProvider}
      customRoutes={customRoutes}
      history={history}
      layout={Layout}
      loginPage={Login}
      logoutButton={Logout}
      {...props}
    >
      {(permissions) => [
        <Resource name="album" {...album} options={{ subMenu: 'albumList' }} />,
        <Resource name="artist" {...artist} />,
        <Resource name="song" {...song} />,
        <Resource
          name="radio"
          {...(permissions === 'admin' ? radio.admin : radio.all)}
        />,
        config.enableSharing && <Resource name="share" {...share} />,
        <Resource
          name="playlist"
          {...playlist}
          options={{ subMenu: 'playlist' }}
        />,
        <Resource name="user" {...user} options={{ subMenu: 'settings' }} />,
        <Resource
          name="player"
          {...player}
          options={{ subMenu: 'settings' }}
        />,
        permissions === 'admin' ? (
          <Resource
            name="transcoding"
            {...transcoding}
            options={{ subMenu: 'settings' }}
          />
        ) : (
          <Resource name="transcoding" />
        ),
        permissions === 'admin' ? (
          <Resource
            name="library"
            {...library}
            options={{ subMenu: 'settings' }}
          />
        ) : null,
        permissions === 'admin' ? (
          <Resource
            name="missing"
            {...missing}
            options={{ subMenu: 'settings' }}
          />
        ) : null,

        <Resource name="translation" />,
        <Resource name="genre" />,
        <Resource name="tag" />,
        <Resource name="playlistTrack" />,
        <Resource name="keepalive" />,
        <Resource name="insights" />,
        <Resource name="config" />,
        <Player />,
      ]}
    </RAAdmin>
  )
  /* eslint-enable react/jsx-key */
}

// AppWithHotkeys 是应用入口。
// 分享页走完全独立的精简播放器，不加载后台管理界面（无需登录）。
const AppWithHotkeys = () => {
  let language = localStorage.getItem('locale') || 'en'
  document.documentElement.lang = language
  if (config.enableSharing && shareInfo) {
    return <SharePlayer />
  }
  return (
    <HotKeys keyMap={keyMap}>
      <DndProvider backend={HTML5Backend}>
        <App />
      </DndProvider>
    </HotKeys>
  )
}

export default AppWithHotkeys
