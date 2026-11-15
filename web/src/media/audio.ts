import type { AudioRoute } from '@/lib/types'
export const audioItems: { value: AudioRoute; label: string }[] = [
 { value: 'muted', label: '静音' }, { value: 'device', label: '设备播放' }, { value: 'browser', label: '网页播放' },
]
export type AudioSnapshot = { route: AudioRoute | 'unknown'; target: AudioRoute; status: 'starting' | 'ready' | 'failed' | ''; ownerSessionId?: string; error?: string; revision: number }
