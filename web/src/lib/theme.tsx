import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react'
import { toast } from '@/components/ui/toast'
import { errorMessage } from './api'

export type ThemeMode = 'light' | 'dark' | 'system'
const key = 'droidharbor.theme'
const ThemeContext = createContext<{ mode: ThemeMode; setMode: (mode: ThemeMode) => void } | null>(null)
function parseMode(value: string | null): ThemeMode {
  if (value === null) return 'system'
  if (value === 'light' || value === 'dark' || value === 'system') return value
  throw new Error('保存的主题设置无效，请清除本站点的主题设置。')
}
export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setTheme] = useState<ThemeMode>(() => parseMode(localStorage.getItem(key)))
  const [systemDark, setSystemDark] = useState(() => matchMedia('(prefers-color-scheme: dark)').matches)
  useEffect(() => {
    const query = matchMedia('(prefers-color-scheme: dark)')
    const update = () => setSystemDark(query.matches)
    query.addEventListener('change', update)
    const receive = (event: StorageEvent) => {
      if (event.storageArea !== localStorage || event.key !== key) return
      try { setTheme(parseMode(event.newValue)) }
      catch (error) { toast.add({ title: '主题设置读取失败', description: errorMessage(error), type: 'error', timeout: 0 }) }
    }
    window.addEventListener('storage', receive)
    return () => { query.removeEventListener('change', update); window.removeEventListener('storage', receive) }
  }, [])
  useEffect(() => {
    const dark = mode === 'dark' || mode === 'system' && systemDark
    document.documentElement.classList.toggle('dark', dark)
    document.documentElement.style.colorScheme = dark ? 'dark' : 'light'
  }, [mode, systemDark])
  const setMode = useCallback((next: ThemeMode) => {
    try { localStorage.setItem(key, next); setTheme(next) }
    catch (error) { toast.add({ title: '主题切换失败', description: errorMessage(error), type: 'error', timeout: 0 }) }
  }, [])
  return <ThemeContext value={{ mode, setMode }}>{children}</ThemeContext>
}
export function useTheme() {
  const theme = useContext(ThemeContext)
  if (!theme) throw new Error('ThemeProvider 未挂载')
  return theme
}
