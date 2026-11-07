import { apiResponse } from './api'

export async function downloadScreenshot(sessionId: string): Promise<void> {
  const response = await apiResponse(`/sessions/${encodeURIComponent(sessionId)}/screenshot`)
  const blob = await response.blob()
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = 'droidharbor-screenshot.png'
  anchor.click()
  window.setTimeout(() => URL.revokeObjectURL(url), 1000)
}
