// Smooth user changes to avoid clicks; this gain affects only browser playback.
export function setWebAudioGain(node: GainNode, volume: number, enhancement: number) {
  node.gain.setTargetAtTime(volume / 100 * enhancement, node.context.currentTime, 0.015)
}
