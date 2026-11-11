import type { RefObject } from 'react'
import type { SessionMode } from '@/lib/types'

export type ControlReply = {
  type: string
  status: string
  code?: string
  message?: string
  payload?: Record<string, unknown>
}

export type MediaSession = {
  sessionId: string | null
  status: 'connecting' | 'connected' | 'closed' | 'failed'
  error: string
  ready: boolean
  hasFrame: boolean
  canRetain: boolean
  epoch: number
  width: number
  height: number
  framesPerSecond: number
  videoRef: RefObject<HTMLVideoElement | null>
  command(type: string, payload?: Record<string, unknown>): Promise<ControlReply>
  release(): void
  suspend(): Promise<void>
  close(): Promise<void>
}

export type { SessionMode }
