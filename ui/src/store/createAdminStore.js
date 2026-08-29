import {
  applyMiddleware,
  combineReducers,
  compose,
  legacy_createStore as createStore,
} from 'redux'
import { routerMiddleware, connectRouter } from 'connected-react-router'
import createSagaMiddleware from 'redux-saga'
import { all, fork } from 'redux-saga/effects'
import { adminReducer, adminSaga, USER_LOGOUT } from 'react-admin'
import throttle from 'lodash.throttle'
import { loadState, saveState } from './persistState'

// createAdminStore 创建 redux store，整合 react-admin、路由与自定义 reducer。
const createAdminStore = ({
  authProvider,
  dataProvider,
  history,
  customReducers = {},
}) => {
  const reducer = combineReducers({
    admin: adminReducer,
    router: connectRouter(history),
    ...customReducers,
  })
  // 登出时把整个 state 置为 undefined，确保不残留上一个用户的数据。
  const resettableAppReducer = (state, action) =>
    reducer(action.type !== USER_LOGOUT ? state : undefined, action)

  const saga = function* rootSaga() {
    yield all([adminSaga(dataProvider, authProvider)].map(fork))
  }
  const sagaMiddleware = createSagaMiddleware()

  const composeEnhancers =
    (process.env.NODE_ENV === 'development' &&
      typeof window !== 'undefined' &&
      window.__REDUX_DEVTOOLS_EXTENSION_COMPOSE__ &&
      window.__REDUX_DEVTOOLS_EXTENSION_COMPOSE__({
        trace: true,
        traceLimit: 25,
      })) ||
    compose

  // 恢复播放位置：savedPlayIndex 是持久化的字段，
  // playIndex 是播放器运行时字段，需在启动时同步一次。
  const persistedState = loadState()
  if (persistedState?.player?.savedPlayIndex) {
    persistedState.player.playIndex = persistedState.player.savedPlayIndex
  }
  const store = createStore(
    resettableAppReducer,
    persistedState,
    composeEnhancers(
      applyMiddleware(sagaMiddleware, routerMiddleware(history)),
    ),
  )

  // 只持久化必要字段（主题、曲库、播放队列等），
  // 并做节流，避免播放进度等高频更新反复写 localStorage。
  store.subscribe(
    throttle(() => {
      const state = store.getState()
      saveState({
        theme: state.theme,
        library: state.library,
        player: (({ queue, volume, savedPlayIndex }) => ({
          queue,
          volume,
          savedPlayIndex,
        }))(state.player),
        albumView: state.albumView,
        settings: state.settings,
      })
    }),
    1000,
  )

  sagaMiddleware.run(saga)
  return store
}

export default createAdminStore
