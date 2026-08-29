import React, { useCallback, useEffect, useMemo, useState } from 'react'
import { useDispatch, useSelector } from 'react-redux'
import { useMediaQuery } from '@material-ui/core'
import { ThemeProvider } from '@material-ui/core/styles'
import {
  createMuiTheme,
  useAuthState,
  useDataProvider,
  useTranslate,
} from 'react-admin'
import ReactGA from 'react-ga'
import { GlobalHotKeys } from 'react-hotkeys'
import ReactJkMusicPlayer from 'navidrome-music-player'
import 'navidrome-music-player/assets/index.css'
import useCurrentTheme from '../themes/useCurrentTheme'
import config from '../config'
import useStyle from './styles'
import AudioTitle from './AudioTitle'
import {
  clearQueue,
  currentPlaying,
  setPlayMode,
  setVolume,
  syncQueue,
} from '../actions'
import PlayerToolbar from './PlayerToolbar'
import { sendNotification } from '../utils'
import subsonic from '../subsonic'
import locale from './locale'
import { keyMap } from '../hotkeys'
import keyHandlers from './keyHandlers'
import { calculateGain } from '../utils/calculateReplayGain'

// Player 是常驻的全局播放器组件。
// 它挂在 react-admin 的资源列表里，从而在切换页面时不被卸载，保证播放不中断。
const Player = () => {
  const theme = useCurrentTheme()
  const translate = useTranslate()
  const playerTheme = theme.player?.theme || 'dark'
  const dataProvider = useDataProvider()
  const playerState = useSelector((state) => state.player)
  const dispatch = useDispatch()
  const [startTime, setStartTime] = useState(null)
  const [scrobbled, setScrobbled] = useState(false)
  const [preloaded, setPreload] = useState(false)
  const [audioInstance, setAudioInstance] = useState(null)
  const isDesktop = useMediaQuery('(min-width:810px)')
  const isMobilePlayer =
    /Android|webOS|iPhone|iPad|iPod|BlackBerry|IEMobile|Opera Mini/i.test(
      navigator.userAgent,
    )

  const { authenticated } = useAuthState()
  const visible = authenticated && playerState.queue.length > 0
  const isRadio = playerState.current?.isRadio || false
  const classes = useStyle({
    isRadio,
    visible,
    enableCoverAnimation: config.enableCoverAnimation,
  })
  const showNotifications = useSelector(
    (state) => state.settings.notifications || false,
  )
  const gainInfo = useSelector((state) => state.replayGain)
  const [context, setContext] = useState(null)
  const [gainNode, setGainNode] = useState(null)

  useEffect(() => {
    if (
      context === null &&
      audioInstance &&
      // Web Audio API 只能对同一个 audio 元素创建一次 source，
      // 故只在首次且确实启用了回放增益时才建立音频链路。
      config.enableReplayGain &&
      'AudioContext' in window &&
      (gainInfo.gainMode === 'album' || gainInfo.gainMode === 'track')
    ) {
      const ctx = new AudioContext()
      // 电台是跨域流，不设置 crossOrigin 会让 Firefox 拒绝接入音频处理链路。
      // we need this to support radios in firefox
      audioInstance.crossOrigin = 'anonymous'
      const source = ctx.createMediaElementSource(audioInstance)
      const gain = ctx.createGain()

      source.connect(gain)
      gain.connect(ctx.destination)

      setContext(ctx)
      setGainNode(gain)
    }
  }, [audioInstance, context, gainInfo.gainMode])

  useEffect(() => {
    if (gainNode) {
      const current = playerState.current || {}
      const song = current.song || {}

      const numericGain = calculateGain(gainInfo, song)
      gainNode.gain.setValueAtTime(numericGain, context.currentTime)
    }
  }, [audioInstance, context, gainNode, playerState, gainInfo])

  const defaultOptions = useMemo(
    () => ({
      theme: playerTheme,
      bounds: 'body',
      playMode: playerState.mode,
      mode: 'full',
      loadAudioErrorPlayNext: false,
      autoPlayInitLoadPlayList: true,
      clearPriorAudioLists: false,
      showDestroy: true,
      showDownload: false,
      showLyric: true,
      showReload: false,
      toggleMode: !isDesktop,
      glassBg: false,
      showThemeSwitch: false,
      showMediaSession: true,
      restartCurrentOnPrev: true,
      quietUpdate: true,
      defaultPosition: {
        top: 300,
        left: 120,
      },
      volumeFade: { fadeIn: 200, fadeOut: 200 },
      renderAudioTitle: (audioInfo, isMobile) => (
        <AudioTitle
          audioInfo={audioInfo}
          gainInfo={gainInfo}
          isMobile={isMobile}
        />
      ),
      locale: locale(translate),
      sortableOptions: { delay: 200, delayOnTouchOnly: true },
    }),
    [gainInfo, isDesktop, playerTheme, translate, playerState.mode],
  )

  const options = useMemo(() => {
    const current = playerState.current || {}
    return {
      ...defaultOptions,
      audioLists: playerState.queue.map((item) => item),
      playIndex: playerState.playIndex,
      autoPlay: playerState.clear || playerState.playIndex === 0,
      clearPriorAudioLists: playerState.clear,
      extendsContent: (
        <PlayerToolbar id={current.trackId} isRadio={current.isRadio} />
      ),
      defaultVolume: isMobilePlayer ? 1 : playerState.volume,
      showMediaSession: !current.isRadio,
    }
  }, [playerState, defaultOptions, isMobilePlayer])

  const onAudioListsChange = useCallback(
    (_, audioLists, audioInfo) => dispatch(syncQueue(audioInfo, audioLists)),
    [dispatch],
  )

  const nextSong = useCallback(() => {
    const idx = playerState.queue.findIndex(
      (item) => item.uuid === playerState.current.uuid,
    )
    return idx !== null ? playerState.queue[idx + 1] : null
  }, [playerState])

  // onAudioProgress 处理播放进度：达到阈值后预加载下一首并上报播放记录。
  // 阈值遵循 Last.fm 惯例——播放过半或超过 4 分钟才算一次有效收听。
  // 电台无固定时长，不参与统计。
  const onAudioProgress = useCallback(
    (info) => {
      if (info.ended) {
        document.title = 'Navidrome'
      }

      const progress = (info.currentTime / info.duration) * 100
      if (isNaN(info.duration) || (progress < 50 && info.currentTime < 240)) {
        return
      }

      if (info.isRadio) {
        return
      }

      if (!preloaded) {
        const next = nextSong()
        if (next != null) {
          const audio = new Audio()
          audio.src = next.musicSrc
        }
        setPreload(true)
        return
      }

      if (!scrobbled) {
        info.trackId && subsonic.scrobble(info.trackId, startTime)
        setScrobbled(true)
      }
    },
    [startTime, scrobbled, nextSong, preloaded],
  )

  // 存平方根值：人耳对音量的感知是对数的，
  // 这样滑块的线性移动才对应听感上均匀的变化。
  const onAudioVolumeChange = useCallback(
    // sqrt to compensate for the logarithmic volume
    (volume) => dispatch(setVolume(Math.sqrt(volume))),
    [dispatch],
  )

  // onAudioPlay 处理开始播放：恢复音频上下文、上报「正在播放」、更新标题与通知。
  const onAudioPlay = useCallback(
    (info) => {
      // Do this to start the context; on chrome-based browsers, the context
      // will start paused since it is created prior to user interaction
      if (context && context.state !== 'running') {
        context.resume()
      }

      dispatch(currentPlaying(info))
      if (startTime === null) {
        setStartTime(Date.now())
      }
      if (info.duration) {
        const song = info.song
        document.title = `${song.title} - ${song.artist} - Navidrome`
        if (!info.isRadio) {
          const pos = startTime === null ? null : Math.floor(info.currentTime)
          subsonic.nowPlaying(info.trackId, pos)
        }
        setPreload(false)
        if (config.gaTrackingId) {
          ReactGA.event({
            category: 'Player',
            action: 'Play song',
            label: `${song.title} - ${song.artist}`,
          })
        }
        if (showNotifications) {
          sendNotification(
            song.title,
            `${song.artist} - ${song.album}`,
            info.cover,
          )
        }
      }
    },
    [context, dispatch, showNotifications, startTime],
  )

  // onAudioPlayTrackChange 切歌时重置计时与上报标记，使新歌重新走一遍统计判定。
  const onAudioPlayTrackChange = useCallback(() => {
    if (scrobbled) {
      setScrobbled(false)
    }
    if (startTime !== null) {
      setStartTime(null)
    }
  }, [scrobbled, startTime])

  const onAudioPause = useCallback(
    (info) => dispatch(currentPlaying(info)),
    [dispatch],
  )

  // onAudioEnded 播放结束时顺带调用 keepalive，
  // 让长时间只听歌不操作界面的会话不至于因空闲而被登出。
  const onAudioEnded = useCallback(
    (currentPlayId, audioLists, info) => {
      setScrobbled(false)
      setStartTime(null)
      dispatch(currentPlaying(info))
      dataProvider
        .getOne('keepalive', { id: info.trackId })
        // eslint-disable-next-line no-console
        .catch((e) => console.log('Keepalive error:', e))
    },
    [dispatch, dataProvider],
  )

  const onCoverClick = useCallback((mode, audioLists, audioInfo) => {
    if (mode === 'full' && audioInfo?.song?.albumId) {
      window.location.href = `#/album/${audioInfo.song.albumId}/show`
    }
  }, [])

  // onBeforeDestroy 拦截播放器的关闭动作：
  // 改为清空队列（reject 阻止组件真正销毁），保持播放器实例常驻。
  const onBeforeDestroy = useCallback(() => {
    return new Promise((resolve, reject) => {
      dispatch(clearQueue())
      reject()
    })
  }, [dispatch])

  if (!visible) {
    document.title = 'Navidrome'
  }

  const handlers = useMemo(
    () => keyHandlers(audioInstance, playerState),
    [audioInstance, playerState],
  )

  // 移动端音量由系统音量键控制，应用内固定为最大，避免两级衰减导致声音过小。
  useEffect(() => {
    if (isMobilePlayer && audioInstance) {
      audioInstance.volume = 1
    }
  }, [isMobilePlayer, audioInstance])

  return (
    <ThemeProvider theme={createMuiTheme(theme)}>
      <ReactJkMusicPlayer
        {...options}
        className={classes.player}
        onAudioListsChange={onAudioListsChange}
        onAudioVolumeChange={onAudioVolumeChange}
        onAudioProgress={onAudioProgress}
        onAudioPlay={onAudioPlay}
        onAudioPlayTrackChange={onAudioPlayTrackChange}
        onAudioPause={onAudioPause}
        onPlayModeChange={(mode) => dispatch(setPlayMode(mode))}
        onAudioEnded={onAudioEnded}
        onCoverClick={onCoverClick}
        onBeforeDestroy={onBeforeDestroy}
        getAudioInstance={setAudioInstance}
      />
      <GlobalHotKeys handlers={handlers} keyMap={keyMap} allowChanges />
    </ThemeProvider>
  )
}

export { Player }
