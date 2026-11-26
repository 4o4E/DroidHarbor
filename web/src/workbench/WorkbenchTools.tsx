import { Clipboard, Copy, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { copyToComputer } from '@/lib/copyToComputer'
import type { Device, SessionMode } from '@/lib/types'
import type { MediaSession } from '@/media/types'
import { useClipboardHistory } from './ClipboardHistory'
import { DeviceManagement } from './DeviceManagement'

type Props = { device: Device; mode: SessionMode; session?: MediaSession }

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
