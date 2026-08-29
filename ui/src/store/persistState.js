// loadState 从 localStorage 恢复状态，解析失败时返回 undefined 走默认初始状态。
export const loadState = () => {
  try {
    const serializedState = localStorage.getItem('state')
    if (serializedState === null) {
      return undefined
    }
    return JSON.parse(serializedState)
  } catch (err) {
    return undefined
  }
}

// saveState 持久化状态，写入失败（如隐私模式或超配额）时静默忽略。
export const saveState = (state) => {
  try {
    const serializedState = JSON.stringify(state)
    localStorage.setItem('state', serializedState)
  } catch (err) {
    // Ignore write errors
  }
}
