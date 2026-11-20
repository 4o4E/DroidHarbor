import { Clipboard, Copy, Trash2 } from 'lucide-react'
import { toast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { errorMessage } from '@/lib/api'
import type { Device, SessionMode } from '@/lib/types'
import type { MediaSession } from '@/media/types'
import { useClipboardHistory } from './ClipboardHistory'
import { DeviceManagement } from './DeviceManagement'

type Props = { device: Device; mode: SessionMode; session?: MediaSession }

async function copyToComputer(text: string) {
  try {
    if (window.isSecureContext && navigator.clipboard) {
      await navigator.clipboard.writeText(text)
    } else {
      // LAN HTTP still supports Chromium's user-gesture copy command.
      const active = document.activeElement
      const field = document.createElement('textarea')
      field.value = text
      field.readOnly = true
      field.className = 'fixed top-0 left-0 size-px opacity-0 pointer-events-none'
      document.body.append(field)
      try {
        field.select()
        if (!document.execCommand('copy')) throw new Error('浏览器拒绝复制到系统剪贴板')
      } finally {
        field.remove()
        if (active instanceof HTMLElement) active.focus({ preventScroll: true })
      }
    }
    toast.add({ title: '已复制到电脑剪贴板', type: 'success' })
  } catch (error) {
    toast.add({ title: '复制到电脑未完成', description: errorMessage(error), type: 'error' })
  }
}

export function WorkbenchTools({ device, mode, session }: Props) {
  const history = useClipboardHistory(device.deviceId)
  const clipboard = <section className="tool-section clipboard-section" aria-labelledby="clipboard-title">
      <div className="flex items-center justify-between">
        <h2 id="clipboard-title"><Clipboard />手机剪贴板</h2>
        <Button size="icon-sm" variant="ghost" aria-label="清空剪贴板历史" title="清空历史" disabled={!history.entries.length} onClick={history.clear}><Trash2 /></Button>
      </div>
      <div className="clipboard-history">
        {history.entries.length ? history.entries.map(entry => <article key={entry.id} className="clipboard-entry">
          <div className="flex items-start justify-between gap-2">
            <p className="line-clamp-4 whitespace-pre-wrap break-words text-sm">{entry.text}</p>
            <Button variant="ghost" size="icon-sm" aria-label="复制到电脑剪贴板" title="复制到电脑" className="shrink-0" onClick={() => void copyToComputer(entry.text)}><Copy /></Button>
          </div>
        </article>) : <p className="py-5 text-center text-sm text-muted-foreground">暂无手机剪贴板记录</p>}
      </div>
    </section>
  return <aside className="workbench-tools" aria-label="工作台工具">
    {mode === 'control'
      ? <DeviceManagement sessionId={session?.sessionId ?? undefined} active={!!session?.canUseDeviceTools}>{clipboard}</DeviceManagement>
      : <div className="min-h-0 flex-1 overflow-auto">{clipboard}</div>}
  </aside>
}
