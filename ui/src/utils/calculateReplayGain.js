// calculateReplayGain 计算音量缩放系数。
// 取增益值与 1/peak 的较小者，确保放大后不会削波失真。
// 缺少增益信息时返回 1（不做调整）。
const calculateReplayGain = (preAmp, gain, peak) => {
  if (gain === undefined || peak === undefined) {
    return 1
  }

  // https://wiki.hydrogenaud.io/index.php?title=ReplayGain_1.0_specification&section=19
  // Normalized to max gain
  return Math.min(10 ** ((gain + preAmp) / 20), 1 / peak)
}

// calculateGain 按当前模式选用专辑增益或单曲增益。
// 专辑模式保留专辑内各曲目的相对响度差异，适合整张连续聆听。
export const calculateGain = (gainInfo, song) => {
  switch (gainInfo.gainMode) {
    case 'album': {
      return calculateReplayGain(
        gainInfo.preAmp,
        song.rgAlbumGain,
        song.rgAlbumPeak,
      )
    }
    case 'track': {
      return calculateReplayGain(
        gainInfo.preAmp,
        song.rgTrackGain,
        song.rgTrackPeak,
      )
    }
    default: {
      return 1
    }
  }
}
