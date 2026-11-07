import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react'
import { QueryClientProvider } from '@tanstack/react-query'
import { api, ApiError, AUTH_INVALIDATED, errorMessage, setCSRFToken } from './api'
import { queryClient } from './query-client'
import type { AdminSession } from './types'

type Auth = {
  session: AdminSession | null
  loading: boolean
  error: string
  login: (token: string) => Promise<void>
  logout: () => Promise<void>
}
const AuthContext = createContext<Auth | null>(null)
export function useAuth() {
  const value = useContext(AuthContext)
  if (!value) throw new Error('AuthProvider 缺失')
  return value
}
export function AuthProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<AdminSession | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const generation = useRef(0)
  const channel = useRef<BroadcastChannel | null>(null)
  const clear = useCallback(() => {
    generation.current++
    setCSRFToken('')
    setSession(null)
    queryClient.clear()
  }, [])
  useEffect(() => {
    const controller = new AbortController()
    let active = true
    async function restore() {
      const current = ++generation.current
      setLoading(true)
      try {
        const restored = await api<AdminSession>('/admin/sessions/current', { signal: controller.signal })
        if (!active || current !== generation.current) return
        setCSRFToken(restored.csrfToken)
        setSession(restored)
        setError('')
      } catch (failure) {
        if (!active || controller.signal.aborted) return
        clear()
        setError(failure instanceof ApiError && failure.status === 401 ? '' : errorMessage(failure))
      } finally { if (active) setLoading(false) }
    }
    const invalidated = () => { clear(); setLoading(false); setError('登录已失效，请重新登录。') }
    window.addEventListener(AUTH_INVALIDATED, invalidated)
    if (typeof BroadcastChannel !== 'undefined') {
      const updates = new BroadcastChannel('droidharbor:administrator-session')
      channel.current = updates
      updates.onmessage = () => { clear(); void restore() }
    }
    void restore()
    return () => {
      active = false
      controller.abort()
      window.removeEventListener(AUTH_INVALIDATED, invalidated)
      channel.current?.close()
      channel.current = null
    }
  }, [clear])
  const login = async (token: string) => {
    const current = ++generation.current
    setError('')
    const loggedIn = await api<AdminSession>('/admin/sessions', { method: 'POST', body: { token } })
    if (current !== generation.current) return
    queryClient.clear()
    setCSRFToken(loggedIn.csrfToken)
    setSession(loggedIn)
    setLoading(false)
    channel.current?.postMessage('changed')
  }
  const logout = async () => {
    await api<void>('/admin/sessions/current', { method: 'DELETE' })
    clear()
    setError('')
    channel.current?.postMessage('changed')
  }
  return <QueryClientProvider client={queryClient}><AuthContext.Provider value={{ session, loading, error, login, logout }}>{children}</AuthContext.Provider></QueryClientProvider>
}
