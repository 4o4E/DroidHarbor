import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Field, FieldDescription, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { toast } from '@/components/ui/toast'
import { api, errorMessage } from '@/lib/api'
import { useSystemPreferences } from '@/lib/queries'
import { useErrorNotification } from '@/lib/notifications'
import type { SystemPreferences } from '@/lib/types'
import { GlobalVideoSettings } from '@/workbench/GlobalVideoSettings'
import { audioItems } from '@/media/audio'

export function SystemSettings() {
 const query = useSystemPreferences()
 const client = useQueryClient()
 useErrorNotification('无法读取系统设置', query.error)
 const [draft, setDraft] = useState<SystemPreferences | null>(null)
 const [scalesText, setScalesText] = useState('')
 const [dirty, setDirty] = useState(false)
 const [saving, setSaving] = useState(false)
 useEffect(() => {
  if (query.data && !dirty) { setDraft(query.data); setScalesText(query.data.windowScales.join(', ')) }
 }, [query.data, dirty])
 const save = async () => {
  if (!draft || saving) return
  const scales = scalesText.split(/[,，\s]+/).filter(Boolean).map(Number)
  if (!scales.length || scales.some(value => !Number.isFinite(value) || value <= 0) || new Set(scales).size !== scales.length) {
   toast.add({ title: '请检查窗口倍率', description: '填写互不重复的正数，用逗号分隔，顺序就是左键切换顺序。', type: 'warning' }); return
  }
  if (!Number.isFinite(draft.audioGainMax) || draft.audioGainMax < 1) {
   toast.add({ title: '请检查音频增强上限', description: '填写至少 1.0 的有限数。', type: 'warning' }); return
  }
  setSaving(true)
  try {
   const saved = await api<SystemPreferences>('/system-preferences', { method: 'PATCH', body: { ...draft, windowScales: scales } })
   client.setQueryData(['system-preferences'], saved)
   setDraft(saved); setDirty(false)
   toast.add({ title: '系统设置已保存', type: 'success' })
  } catch (error) {
   toast.add({ title: '系统设置未保存', description: errorMessage(error), type: 'error', timeout: 0, actionProps: { children: '重新读取', onClick: () => { void query.refetch({ throwOnError: true }).then(result => { if (result.data) { setDraft(result.data); setScalesText(result.data.windowScales.join(', ')); setDirty(false) } }).catch(cause => toast.add({ title: '无法重新读取系统设置', description: errorMessage(cause), type: 'error', timeout: 0 })) } } })
  } finally { setSaving(false) }
 }
 return <section className="flex max-w-4xl flex-col gap-6"><h1 className="text-2xl font-semibold tracking-tight">系统设置</h1><div className="grid gap-4 lg:grid-cols-2">
  <Card size="sm"><CardContent><GlobalVideoSettings /></CardContent></Card>
  <Card size="sm"><CardHeader><CardTitle>预览默认值</CardTitle></CardHeader><CardContent><FieldGroup>
   <Field><FieldLabel htmlFor="audio-default">音频播放位置</FieldLabel><Select items={audioItems} value={draft?.audioDefault ?? null} disabled={!draft || saving} onValueChange={value => { if (draft && value) { setDraft({ ...draft, audioDefault: value as SystemPreferences['audioDefault'] }); setDirty(true) } }}><SelectTrigger id="audio-default" className="w-full"><SelectValue placeholder="读取中" /></SelectTrigger><SelectContent><SelectGroup>{audioItems.map(item => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}</SelectGroup></SelectContent></Select><FieldDescription>下次新建投屏生效。</FieldDescription></Field>
   <Field><FieldLabel htmlFor="window-scales">小窗倍率档位</FieldLabel><Input id="window-scales" value={scalesText} disabled={!draft || saving} onChange={event => { setScalesText(event.target.value); setDirty(true) }} placeholder="0.5, 1, 2" /><FieldDescription>按填写顺序切换，新小窗使用第一档。1×对应视频原始像素。</FieldDescription></Field>
   <Field><FieldLabel htmlFor="audio-gain-max">网页音频增强上限</FieldLabel><Input id="audio-gain-max" type="number" min={1} step="0.1" value={draft && Number.isFinite(draft.audioGainMax) ? draft.audioGainMax : ''} disabled={!draft || saving} onChange={event => { if (draft) { setDraft({ ...draft, audioGainMax: event.target.valueAsNumber }); setDirty(true) } }} /><FieldDescription>默认 3.0 倍；1.0 为原声，过高可能失真。</FieldDescription></Field>
   <Button disabled={!dirty || saving || !draft} onClick={() => void save()}>{saving ? '保存中…' : '保存预览默认值'}</Button>
  </FieldGroup></CardContent></Card>
 </div></section>
}
