import { CHANGE_GAIN, CHANGE_PREAMP } from '../actions'

const GAIN_KEY = 'gainMode'
const PREAMP_KEY = 'preAmp'

// getPreAmp 读取预增益，数值非法时退回 0，避免污染音量计算。
const getPreAmp = () => {
  const storage = localStorage.getItem(PREAMP_KEY)

  if (storage === null) {
    return 0
  } else {
    const asFloat = parseFloat(storage)
    return isNaN(asFloat) ? 0 : asFloat
  }
}

const initialState = {
  gainMode: localStorage.getItem(GAIN_KEY) || 'none',
  preAmp: getPreAmp(),
}

// replayGainReducer 管理回放增益设置。
// 这两项直接写 localStorage 而非走统一的持久化：
// 播放器初始化时就要读到，早于 redux store 恢复。
export const replayGainReducer = (
  previousState = initialState,
  { type, payload },
) => {
  switch (type) {
    case CHANGE_GAIN: {
      localStorage.setItem(GAIN_KEY, payload)
      return {
        ...previousState,
        gainMode: payload,
      }
    }
    case CHANGE_PREAMP: {
      const value = parseFloat(payload)
      if (isNaN(value)) {
        return previousState
      }
      localStorage.setItem(PREAMP_KEY, payload)
      return {
        ...previousState,
        preAmp: value,
      }
    }
    default:
      return previousState
  }
}
