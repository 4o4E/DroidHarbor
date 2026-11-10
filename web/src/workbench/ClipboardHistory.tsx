import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react'

export type ClipboardSource = 'phone' | 'browser'
type ClipboardEntry = { id: number; text: string; source: ClipboardSource }
type HistoryState = Record<string, ClipboardEntry[]>
const Context = createContext<{ entries: HistoryState; record: (deviceId: string, text: string, source: ClipboardSource) => void; clear: (deviceId: string) => void } | null>(null)

// Authenticated page memory only: no browser/system clipboard writes or storage.
export function ClipboardHistoryProvider({ children }: { children: ReactNode }) {
  const [entries, setEntries] = useState<HistoryState>({})
  const record = useCallback((deviceId: string, text: string, source: ClipboardSource) => {
    if (!text) return
    setEntries(current => {
      const previous = current[deviceId] ?? []
      if (previous[0]?.text === text) {
        if (source === 'browser' && previous[0].source !== 'browser') {
          return { ...current, [deviceId]: [{ ...previous[0], source }, ...previous.slice(1)] }
        }
        return current
      }
      const id = (previous[0]?.id ?? 0) + 1
      return { ...current, [deviceId]: [{ id, text, source }, ...previous].slice(0, 20) }
    })
  }, [])
  const clear = useCallback((deviceId: string) => setEntries(current => ({ ...current, [deviceId]: [] })), [])
  const value = useMemo(() => ({ entries, record, clear }), [entries, record, clear])
  return <Context value={value}>{children}</Context>
}
export function useClipboardHistory(deviceId: string) {
  const history = useContext(Context)
  if (!history) throw new Error('ClipboardHistoryProvider 未挂载')
  const record = useCallback((text: string, source: ClipboardSource) => history.record(deviceId, text, source), [deviceId, history.record])
  return { entries: history.entries[deviceId] ?? [], record, clear: () => history.clear(deviceId) }
}
