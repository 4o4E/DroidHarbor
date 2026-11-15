export const PHONE_TOOLBAR_WIDTH = 44

// Multipliers describe original video pixels; viewport fitting is a separate
// constraint and never changes the requested multiplier or crops the picture.
export function compactWindowSize(width: number, height: number, scale: number) {
  const chromeWidth = Math.max(0, window.outerWidth - window.innerWidth)
  const chromeHeight = Math.max(0, window.outerHeight - window.innerHeight)
  const requestedWidth = width * scale
  const requestedHeight = height * scale
  const fit = Math.min(1, Math.max(0, window.screen.availWidth - chromeWidth - PHONE_TOOLBAR_WIDTH) / requestedWidth, Math.max(0, window.screen.availHeight - chromeHeight) / requestedHeight)
  return { innerWidth: Math.floor(requestedWidth * fit) + PHONE_TOOLBAR_WIDTH, innerHeight: Math.floor(requestedHeight * fit), chromeWidth, chromeHeight, fitted: fit < 1 }
}
