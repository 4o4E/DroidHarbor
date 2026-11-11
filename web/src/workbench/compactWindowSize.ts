export const PHONE_TOOLBAR_WIDTH = 44

// Height first; constrain only when the resulting width would leave the display.
// Calibrate the popup's browser chrome from saved geometry, before video decoding.
export function compactWindowSize(ratio: number, fraction: number) {
  const chromeWidth = Math.max(0, window.outerWidth - window.innerWidth)
  const chromeHeight = Math.max(0, window.outerHeight - window.innerHeight)
  const height = Math.floor(Math.min(window.screen.availHeight - chromeHeight, (window.screen.availWidth - chromeWidth - PHONE_TOOLBAR_WIDTH) / ratio) * fraction)
  return { innerWidth: Math.ceil(height * ratio) + PHONE_TOOLBAR_WIDTH, innerHeight: height, chromeWidth, chromeHeight }
}
