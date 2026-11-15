import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Settings2 } from 'lucide-react'
import { toast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Field, FieldLabel } from '@/components/ui/field'
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { api, ApiError, errorMessage } from '@/lib/api'
import { useGlobalVideoSettings } from '@/lib/queries'
import type { GlobalVideoSnapshot, SessionMode, VideoQuality } from '@/lib/types'

const settingsKey = ['global-video-settings']
const frameRates = [30, 60, 90, 120]
const bitRates = [4, 8, 12, 20, 40].map(value => value * 1_000_000)

export function GlobalVideoSettings({ mode = 'control', sessionId }: { mode?: SessionMode; sessionId?: string }) {
  const settings = useGlobalVideoSettings()
  const client = useQueryClient()
  const [draft, setDraft] = useState<VideoQuality | null>(null)
  const [dirty, setDirty] = useState<Partial<VideoQuality>>({})
  const [baseRevision, setBaseRevision] = useState<number | null>(null)
  const [saving, setSaving] = useState(false)
  const editable = mode === 'control'
  useEffect(() => {
    if (settings.data && Object.keys(dirty).length === 0) {
      setDraft(settings.data.requested)
      setBaseRevision(null)
    }
  }, [settings.data, dirty])
  const editQuality = (key: keyof VideoQuality, value: number) => {
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
      toast.add({ title: '全局画质已重新读取', type: 'success' })
    } catch (error) { toast.add({ title: '无法读取全局画质', description: errorMessage(error), type: 'error', timeout: 0 }) }
  }
  const saveQuality = async () => {
    if (!settings.data || !Object.keys(dirty).length || !editable) return
    setSaving(true)
    try {
      const snapshot = await api<GlobalVideoSnapshot>('/video-settings', {
        method: 'PATCH', body: { sessionId, revision: baseRevision ?? settings.data.revision, ...dirty },
      })
      client.setQueryData(settingsKey, snapshot)
      setDraft(snapshot.requested); setDirty({}); setBaseRevision(null)
      toast.add({ title: '全局画质已保存，下次投屏使用', type: 'success' })
    } catch (error) {
      toast.add({ title: '全局画质未保存', description: errorMessage(error), type: 'error', timeout: 0,
        actionProps: error instanceof ApiError && error.status === 409 ? { children: '读取最新设置', onClick: () => void refreshQuality() } : undefined })
    } finally { setSaving(false) }
  }
  const fpsItems = [...new Set([...frameRates, ...(draft ? [draft.maxFps] : [])])].sort((a, b) => a - b).map(value => ({ value: String(value), label: `${value} fps` }))
  const rateItems = [...new Set([...bitRates, ...(draft ? [draft.bitRate] : [])])].sort((a, b) => a - b).map(value => ({ value: String(value), label: `${value / 1_000_000} Mbps` }))
  return <section className="tool-section" aria-labelledby="quality-title">
    <h2 id="quality-title"><Settings2 />全局画质</h2>
    <Field><FieldLabel htmlFor="frame-rate">帧率上限</FieldLabel><Select items={fpsItems} value={draft ? String(draft.maxFps) : null} disabled={!draft || !editable || saving} onValueChange={value => { if (value) editQuality('maxFps', Number(value)) }}><SelectTrigger id="frame-rate" className="w-full"><SelectValue placeholder="读取中" /></SelectTrigger><SelectContent><SelectGroup>{fpsItems.map(item => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}</SelectGroup></SelectContent></Select></Field>
    <Field><FieldLabel htmlFor="bit-rate">码率</FieldLabel><Select items={rateItems} value={draft ? String(draft.bitRate) : null} disabled={!draft || !editable || saving} onValueChange={value => { if (value) editQuality('bitRate', Number(value)) }}><SelectTrigger id="bit-rate" className="w-full"><SelectValue placeholder="读取中" /></SelectTrigger><SelectContent><SelectGroup>{rateItems.map(item => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}</SelectGroup></SelectContent></Select></Field>
    <Button className="w-full" variant="outline" size="sm" title="保存全局设置，保留当前视频" disabled={!editable || saving || !Object.keys(dirty).length} onClick={() => void saveQuality()}>{saving ? '保存中…' : '保存全局画质'}</Button>
    {settings.isError && <Button variant="outline" size="sm" onClick={() => void refreshQuality()}>重新读取画质</Button>}
  </section>
}
