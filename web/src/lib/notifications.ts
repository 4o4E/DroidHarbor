import { useEffect, useEffectEvent } from 'react'
import { toast } from '@/components/ui/toast'
import { ApiError, errorMessage } from './api'

// A polling failure keeps its first request ID until recovery or a new error.
export function useErrorNotification(title: string, error: unknown) {
  const message = error == null || error === '' ? '' : typeof error === 'string' ? error : errorMessage(error)
  const key = !message ? '' : error instanceof ApiError
    ? `${error.status}:${error.code}:${error.message}`
    : error instanceof Error ? `${error.name}:${error.message}` : message
  const addNotice = useEffectEvent(() => toast.add({ title, description: message, type: 'error', timeout: 0 }))
  useEffect(() => {
    if (!key) return
    const id = addNotice()
    return () => { toast.close(id) }
  }, [title, key])
}
