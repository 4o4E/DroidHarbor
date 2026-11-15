import { useState } from 'react'
import { Clipboard, ClipboardPaste, Trash2 } from 'lucide-react'
import { toast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { errorMessage } from '@/lib/api'
import type { Device, SessionMode } from '@/lib/types'
import type { MediaSession } from '@/media/types'
import { useClipboardHistory } from './ClipboardHistory'

type Props = { device: Device; mode: SessionMode; session?: MediaSession }

export function WorkbenchTools({ device, mode, session }: Props) {
  const history = useClipboardHistory(device.deviceId)
  const [pasting, setPasting] = useState(false)
  const editable = mode === 'control' && session?.status === 'connected' && !!session.sessionId
  const paste = async (text: string) => {
    if (!session || !editable || !session.ready || pasting) return
    setPasting(true)
    try {
      const reply = await session.command('clipboard.paste', { text })
      if (reply.status !== 'acknowledged') throw new Error(reply.message || reply.code || `粘贴结果：${reply.status}`)
      toast.add({ title: '剪贴板内容已发送到手机', type: 'success' })
    } catch (error) { toast.add({ title: '粘贴未完成', description: errorMessage(error), type: 'error', timeout: 0 }) }
    finally { setPasting(false) }
  }
  return <aside className="workbench-tools" aria-label="剪贴板">
    <section className="tool-section clipboard-section" aria-labelledby="clipboard-title">
      <div className="flex items-center justify-between"><h2 id="clipboard-title"><Clipboard />剪贴板</h2><Button size="icon-sm" variant="ghost" aria-label="清空剪贴板历史" title="清空历史" disabled={!history.entries.length} onClick={history.clear}><Trash2 /></Button></div>
      <div className="clipboard-history">{history.entries.length ? history.entries.map(entry => <article key={entry.id} className="clipboard-entry"><div className="flex items-center justify-between gap-2"><span className="text-xs text-muted-foreground">{entry.source === 'phone' ? '手机剪贴板' : '发送到手机'}</span><Button variant="ghost" size="icon-sm" aria-label="粘贴到手机" title="粘贴到手机" disabled={!editable || !session?.ready || pasting} onClick={() => void paste(entry.text)}><ClipboardPaste /></Button></div><p className="line-clamp-4 whitespace-pre-wrap break-words text-sm">{entry.text}</p></article>) : <p className="py-5 text-center text-sm text-muted-foreground">暂无剪贴板记录</p>}</div>
    </section>
  </aside>
}
