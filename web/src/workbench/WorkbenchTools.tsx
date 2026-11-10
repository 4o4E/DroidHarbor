import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Clipboard, ClipboardPaste, Settings2, Trash2 } from 'lucide-react'
import { toast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Field, FieldLabel } from '@/components/ui/field'
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { api, ApiError, errorMessage } from '@/lib/api'
import { useVideoSettings } from '@/lib/queries'
import { useErrorNotification } from '@/lib/notifications'
import type { Device, SessionMode, VideoParameters, VideoSnapshot } from '@/lib/types'
import type { MediaSession } from '@/media/types'
import { useClipboardHistory } from './ClipboardHistory'

const settingsKey = (deviceId: string) => ['video-settings', deviceId]
const frameRates = [30, 60, 90, 120]
const bitRates = [4, 8, 12, 20, 40].map(value => value * 1_000_000)
type Props = { device: Device; mode: SessionMode; session: MediaSession }

export function WorkbenchTools({ device, mode, session }: Props) {
  const settings = useVideoSettings(device.deviceId)
  const client = useQueryClient()
  const history = useClipboardHistory(device.deviceId)
  const [draft, setDraft] = useState<VideoParameters | null>(null)
  const [dirty, setDirty] = useState<Partial<VideoParameters>>({})
  const [baseRevision, setBaseRevision] = useState<number | null>(null)
  const [saving, setSaving] = useState(false)
  const [pasting, setPasting] = useState(false)
  useErrorNotification('无法读取画质设置', settings.error)
  useEffect(() => {
    if (settings.data && Object.keys(dirty).length === 0) {
      setDraft(settings.data.requested)
      setBaseRevision(null)
    }
  }, [settings.data, dirty])
  const editable = mode === 'control' && session.status === 'connected' && !!session.sessionId
  const editQuality = (key: 'maxFps' | 'bitRate', value: number) => {
    if (!settings.data || !draft) return
    if (baseRevision === null) setBaseRevision(settings.data.revision)
    const next = { ...draft, [key]: value }
    setDraft(next)
    setDirty(current => {
      const changed = { ...current }
      if (next[key] === settings.data!.requested[key]) delete changed[key]
      else changed[key] = next[key]
      return changed
    })
  }
  const refreshQuality = async () => {
    try {
      const result = await settings.refetch({ throwOnError: true })
      if (result.data) setDraft(result.data.requested)
      setDirty({}); setBaseRevision(null)
      toast.add({ title: '画质设置已重新读取', type: 'success' })
    } catch (error) { toast.add({ title: '无法读取画质设置', description: errorMessage(error), type: 'error', timeout: 0 }) }
  }
  const saveQuality = async () => {
    if (!settings.data || !Object.keys(dirty).length || !editable) return
    setSaving(true)
    try {
      const snapshot = await api<VideoSnapshot>(`/devices/${encodeURIComponent(device.deviceId)}/video-settings`, {
        method: 'PATCH', body: { sessionId: session.sessionId, revision: baseRevision ?? settings.data.revision, ...dirty },
      })
      client.setQueryData(settingsKey(device.deviceId), snapshot)
      setDraft(snapshot.requested); setDirty({}); setBaseRevision(null)
      toast.add({ title: '画质设置已提交', type: 'success' })
    } catch (error) {
      toast.add({ title: '画质设置未完成', description: errorMessage(error), type: 'error', timeout: 0,
        actionProps: error instanceof ApiError && error.status === 409 ? { children: '读取最新设置', onClick: () => void refreshQuality() } : undefined })
    } finally { setSaving(false) }
  }
  const paste = async (text: string) => {
    if (!editable || !session.ready || pasting) return
    setPasting(true)
    try {
      const reply = await session.command('clipboard.paste', { text })
      if (reply.status !== 'acknowledged') throw new Error(reply.message || reply.code || `粘贴结果：${reply.status}`)
      toast.add({ title: '剪贴板内容已发送到手机', type: 'success' })
    } catch (error) { toast.add({ title: '粘贴未完成', description: errorMessage(error), type: 'error', timeout: 0 }) }
    finally { setPasting(false) }
  }
  const fpsItems = [...new Set([...frameRates, ...(draft ? [draft.maxFps] : [])])].sort((a, b) => a - b).map(value => ({ value: String(value), label: `${value} fps` }))
  const rateItems = [...new Set([...bitRates, ...(draft ? [draft.bitRate] : [])])].sort((a, b) => a - b).map(value => ({ value: String(value), label: `${value / 1_000_000} Mbps` }))
  return <aside className="workbench-tools" aria-label="画质和剪贴板">
    <section className="tool-section" aria-labelledby="quality-title">
      <h2 id="quality-title"><Settings2 />画质</h2>
      <Field><FieldLabel htmlFor="frame-rate">帧率</FieldLabel><Select items={fpsItems} value={draft ? String(draft.maxFps) : null} disabled={!draft || !editable || saving} onValueChange={value => { if (value) editQuality('maxFps', Number(value)) }}><SelectTrigger id="frame-rate" className="w-full"><SelectValue placeholder="读取中" /></SelectTrigger><SelectContent><SelectGroup>{fpsItems.map(item => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}</SelectGroup></SelectContent></Select></Field>
      <Field><FieldLabel htmlFor="bit-rate">码率</FieldLabel><Select items={rateItems} value={draft ? String(draft.bitRate) : null} disabled={!draft || !editable || saving} onValueChange={value => { if (value) editQuality('bitRate', Number(value)) }}><SelectTrigger id="bit-rate" className="w-full"><SelectValue placeholder="读取中" /></SelectTrigger><SelectContent><SelectGroup>{rateItems.map(item => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}</SelectGroup></SelectContent></Select></Field>
      <Button className="w-full" variant="outline" size="sm" title="应用到此设备的全部观看窗口" disabled={!editable || saving || !Object.keys(dirty).length} onClick={() => void saveQuality()}>{saving ? '应用中…' : '应用画质'}</Button>
      {settings.isError && <Button variant="outline" size="sm" onClick={() => void refreshQuality()}>重新读取画质</Button>}
    </section>
    <section className="tool-section clipboard-section" aria-labelledby="clipboard-title">
      <div className="flex items-center justify-between"><h2 id="clipboard-title"><Clipboard />剪贴板</h2><Button size="icon-sm" variant="ghost" aria-label="清空剪贴板历史" title="清空历史" disabled={!history.entries.length} onClick={history.clear}><Trash2 /></Button></div>
      <div className="clipboard-history">{history.entries.length ? history.entries.map(entry => <article key={entry.id} className="clipboard-entry"><div className="flex items-center justify-between gap-2"><span className="text-xs text-muted-foreground">{entry.source === 'phone' ? '手机剪贴板' : '发送到手机'}</span><Button variant="ghost" size="icon-sm" aria-label="粘贴到手机" title="粘贴到手机" disabled={!editable || !session.ready || pasting} onClick={() => void paste(entry.text)}><ClipboardPaste /></Button></div><p className="line-clamp-4 whitespace-pre-wrap break-words text-sm">{entry.text}</p></article>) : <p className="py-5 text-center text-sm text-muted-foreground">暂无剪贴板记录</p>}</div>
    </section>
  </aside>
}
