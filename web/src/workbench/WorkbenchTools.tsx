import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { toast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Field, FieldDescription, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Slider } from '@/components/ui/slider'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { api, apiResponse, ApiError, errorMessage } from '@/lib/api'
import { useVideoSettings } from '@/lib/queries'
import { useErrorNotification } from '@/lib/notifications'
import type { Device, SessionMode, Transfer, VideoParameters, VideoSnapshot } from '@/lib/types'
import type { MediaSession } from '@/media/types'
import { Clipboard, FileUp, Keyboard, Settings2 } from 'lucide-react'


const transferStateLabels: Record<Transfer['state'], string> = {
  uploading: '上传中', transferring: '写入设备中', installing: '安装中',
  succeeded: '成功', failed: '失败', cancelled: '已取消', unknown: '结果未知',
}
const captureStatusLabels: Record<VideoSnapshot['status'], string> = {
  idle: '空闲', pending: '等待配置生效', starting: '启动中', running: '运行中',
  reconfiguring: '重新配置中', stopping: '停止中', failed: '失败',
}
const terminalTransfer = (transfer: Transfer) => ['succeeded', 'failed', 'cancelled', 'unknown'].includes(transfer.state)
const settingsKey = (deviceId: string) => ['video-settings', deviceId]

type WorkbenchToolsProps = { device: Device; mode: SessionMode; session: MediaSession }
export function WorkbenchTools({ device, mode, session }: WorkbenchToolsProps) {
  const settings = useVideoSettings(device.deviceId)
  const client = useQueryClient()
  const [draft, setDraft] = useState<VideoParameters | null>(null)
  const [dirty, setDirty] = useState<Partial<VideoParameters>>({})
  const [baseRevision, setBaseRevision] = useState<number | null>(null)
  const [saving, setSaving] = useState(false)
  const [text, setText] = useState('')
  const [clipboardText, setClipboardText] = useState('')
  const [clipboardRead, setClipboardRead] = useState(false)
  const [commandBusy, setCommandBusy] = useState(false)
  const [file, setFile] = useState<File | null>(null)
  const fileInput = useRef<HTMLInputElement>(null)
  const [purpose, setPurpose] = useState<'file' | 'apk'>('file')
  const [targetPath, setTargetPath] = useState('')
  const [overwrite, setOverwrite] = useState(false)
  const [transfer, setTransfer] = useState<Transfer | null>(null)
  const [uploadBusy, setUploadBusy] = useState(false)
  const [cancelBusy, setCancelBusy] = useState(false)
  const [refreshBusy, setRefreshBusy] = useState(false)
  const [transferError, setTransferError] = useState<unknown>(null)
  const uploadController = useRef<AbortController | null>(null)
  const transferQueryController = useRef<AbortController | null>(null)
  const lastTransferNotice = useRef('')
  useErrorNotification('无法读取画质设置', settings.error)
  useErrorNotification('无法读取传输状态', transferError)

  useEffect(() => {
    if (!transfer || !terminalTransfer(transfer)) return
    const key = `${transfer.transferId}:${transfer.state}`
    if (lastTransferNotice.current === key) return
    lastTransferNotice.current = key
    const failed = transfer.state === 'failed' || transfer.state === 'unknown'
    toast.add({
      title: `传输结果：${transferStateLabels[transfer.state]}`,
      description: transfer.error || transfer.result || undefined,
      type: failed ? 'error' : transfer.state === 'succeeded' ? 'success' : 'info',
      timeout: failed ? 0 : undefined,
    })
  }, [transfer])

  useEffect(() => {
    if (settings.data && Object.keys(dirty).length === 0) setDraft(settings.data.requested)
  }, [settings.data, dirty])

  useEffect(() => {
    if (!transfer || terminalTransfer(transfer)) return
    const controller = new AbortController()
    let active = true
    let timer: number | undefined
    const refresh = async () => {
      try {
        const next = await api<Transfer>(`/transfers/${encodeURIComponent(transfer.transferId)}`, { signal: controller.signal })
        if (active) {
          setTransfer(next)
          setTransferError(null)
        }
      } catch (error) {
        if (active && !controller.signal.aborted) setTransferError(error)
      } finally {
        if (active && !controller.signal.aborted) timer = window.setTimeout(() => void refresh(), 1000)
      }
    }
    void refresh()
    return () => { active = false; controller.abort(); if (timer !== undefined) window.clearTimeout(timer) }
  }, [transfer?.transferId, transfer?.state])

  useEffect(() => () => {
    uploadController.current?.abort()
    transferQueryController.current?.abort()
  }, [session.sessionId, session.status])

  const editQuality = (key: keyof VideoParameters, value: number) => {
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

  const saveQuality = async () => {
    if (!settings.data || !Object.keys(dirty).length || mode !== 'control' || !session.sessionId) return
    setSaving(true)
    try {
      const snapshot = await api<VideoSnapshot>(`/devices/${encodeURIComponent(device.deviceId)}/video-settings`, {
        method: 'PATCH', body: { sessionId: session.sessionId, revision: baseRevision ?? settings.data.revision, ...dirty },
      })
      client.setQueryData(settingsKey(device.deviceId), snapshot)
      setDraft(snapshot.requested)
      setDirty({})
      setBaseRevision(null)
      toast.add({ title: '画质设置已提交', type: 'success' })
    } catch (error) {
      toast.add({
        title: '画质设置未完成', description: errorMessage(error), type: 'error', timeout: 0,
        actionProps: error instanceof ApiError && error.status === 409 ? { children: '读取最新设置', onClick: () => void refreshQuality() } : undefined,
      })
    } finally { setSaving(false) }
  }

  const refreshQuality = async () => {
    try {
      await settings.refetch({ throwOnError: true })
      setDirty({})
      setBaseRevision(null)
      toast.add({ title: '画质设置已重新读取', type: 'success' })
    } catch (error) { toast.add({ title: '无法读取画质设置', description: errorMessage(error), type: 'error', timeout: 0 }) }
  }

  const command = async (type: string, payload?: Record<string, unknown>) => {
    setCommandBusy(true)
    try {
      const reply = await session.command(type, payload)
      if (!['succeeded', 'waiting-key-frame', 'dispatched', 'received', 'acknowledged'].includes(reply.status)) throw new Error(reply.message || reply.code || `设备拒绝了 ${reply.type} 操作（${reply.status}）`)
      const feedback = reply.status === 'dispatched' ? '命令已派发，尚未确认手机端完成。' : reply.status === 'acknowledged' ? '设备已确认收到命令；这不表示目标应用已应用内容。' : reply.status === 'received' ? '已收到设备剪贴板内容。' : ''
      if (feedback) toast.add({ title: '设备操作', description: feedback, type: 'info' })
      return reply
    } finally { setCommandBusy(false) }
  }

  const startTransfer = async () => {
    if (!file || mode !== 'control' || !session.sessionId || uploadBusy || cancelBusy || refreshBusy || (transfer && !terminalTransfer(transfer))) return
    const controller = new AbortController()
    uploadController.current = controller
    setUploadBusy(true)
    setTransferError(null)
    try {
      const created = await api<Transfer>(`/sessions/${encodeURIComponent(session.sessionId)}/transfers`, {
        method: 'POST', body: {
          fileName: file.name, purpose, size: file.size,
          ...(purpose === 'file' ? { ...(targetPath.trim() ? { targetPath: targetPath.trim() } : {}), overwrite } : {}),
        },
        signal: controller.signal,
      })
      if (controller.signal.aborted) return
      setTransfer(created)
      const response = await apiResponse(`/transfers/${encodeURIComponent(created.transferId)}/content`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/octet-stream' },
        body: file,
        signal: controller.signal,
      })
      if (controller.signal.aborted) return
      const result = await response.json() as Transfer
      if (controller.signal.aborted) return
      setTransfer(result)
      setFile(null)
      if (fileInput.current) fileInput.current.value = ''
    } catch (error) {
      if (!controller.signal.aborted) toast.add({ title: '上传或传输未完成', description: errorMessage(error), type: 'error', timeout: 0 })
    } finally {
      if (uploadController.current === controller) uploadController.current = null
      setUploadBusy(false)
    }
  }

  const cancelTransfer = async () => {
    if (!transfer || cancelBusy || terminalTransfer(transfer)) return
    uploadController.current?.abort()
    transferQueryController.current?.abort()
    const controller = new AbortController()
    transferQueryController.current = controller
    setCancelBusy(true)
    setTransferError(null)
    try {
      await api<void>(`/transfers/${encodeURIComponent(transfer.transferId)}`, { method: 'DELETE' })
      if (controller.signal.aborted) return
      const next = await api<Transfer>(`/transfers/${encodeURIComponent(transfer.transferId)}`, { signal: controller.signal })
      if (!controller.signal.aborted) setTransfer(next)
    } catch (error) {
      if (!controller.signal.aborted) toast.add({ title: '取消状态未确认', description: errorMessage(error), type: 'error', timeout: 0 })
    } finally {
      if (transferQueryController.current === controller) transferQueryController.current = null
      setCancelBusy(false)
    }
  }
  const copyPhoneClipboardToBrowser = async () => {
    try {
      if (!navigator.clipboard?.writeText) throw new Error('当前浏览器不支持复制到系统剪贴板。')
      await navigator.clipboard.writeText(clipboardText)
      toast.add({ title: '设备文本已复制到电脑剪贴板', type: 'success' })
    } catch (error) { toast.add({ title: '复制到电脑剪贴板失败', description: errorMessage(error), type: 'error', timeout: 0 }) }
  }


  const refreshTransfer = async () => {
    if (!transfer || refreshBusy || cancelBusy) return
    transferQueryController.current?.abort()
    const controller = new AbortController()
    transferQueryController.current = controller
    setRefreshBusy(true)
    setTransferError(null)
    try {
      const next = await api<Transfer>(`/transfers/${encodeURIComponent(transfer.transferId)}`, { signal: controller.signal })
      if (!controller.signal.aborted) setTransfer(next)
    } catch (error) {
      if (!controller.signal.aborted) toast.add({ title: '无法读取传输结果', description: errorMessage(error), type: 'error', timeout: 0 })
    } finally {
      if (transferQueryController.current === controller) transferQueryController.current = null
      setRefreshBusy(false)
    }
  }


  const readonly = mode !== 'control' || !session.sessionId || session.status !== 'connected' || !session.ready
  const transferLocked = readonly || uploadBusy || cancelBusy || refreshBusy || (!!transfer && !terminalTransfer(transfer))
  const current = settings.data
  const parameters = draft ?? current?.requested
  const dirtyKeys = Object.keys(dirty).length > 0

  return (
    <Tabs defaultValue="quality">
      <TabsList className="w-full"><TabsTrigger value="quality"><Settings2 />画质</TabsTrigger><TabsTrigger value="text"><Clipboard />文本</TabsTrigger><TabsTrigger value="files"><FileUp />文件</TabsTrigger></TabsList>
      <TabsContent value="quality" className="pt-4">
        <FieldGroup>
          {settings.isLoading && <p className="text-sm text-muted-foreground">正在读取画质设置…</p>}
          {parameters && <>
            <Field><FieldLabel htmlFor="resolution">长边上限 <span className="ml-auto tabular-nums">{parameters.maxSize} px</span></FieldLabel><Slider id="resolution" aria-label="长边上限" value={[parameters.maxSize]} min={480} max={2560} step={16} disabled={readonly || saving} onValueChange={value => editQuality('maxSize', Array.isArray(value) ? value[0] : value)} /><FieldDescription>保持手机画面比例</FieldDescription></Field>
            <Field><FieldLabel htmlFor="fps">帧率上限 <span className="ml-auto tabular-nums">{parameters.maxFps} fps</span></FieldLabel><Slider id="fps" aria-label="帧率上限" value={[parameters.maxFps]} min={5} max={60} step={1} disabled={readonly || saving} onValueChange={value => editQuality('maxFps', Array.isArray(value) ? value[0] : value)} /></Field>
            <Field><FieldLabel htmlFor="bitrate">目标码率 <span className="ml-auto tabular-nums">{(parameters.bitRate / 1_000_000).toFixed(1)} Mbps</span></FieldLabel><Slider id="bitrate" aria-label="目标码率" value={[Math.min(12, Math.max(1, parameters.bitRate / 1_000_000))]} min={1} max={12} step={1} disabled={readonly || saving} onValueChange={value => editQuality('bitRate', (Array.isArray(value) ? value[0] : value) * 1_000_000)} /><FieldDescription>修改将影响此设备的所有观看者</FieldDescription></Field>
            {current?.effective && <p className="text-xs text-muted-foreground">实际 {current.effective.width} × {current.effective.height} · {current.effective.framesPerSecond.toFixed(1)} fps · {(current.effective.encodedBitsPerSecond / 1_000_000).toFixed(2)} Mbps</p>}
            {current && <p className="text-xs text-muted-foreground">采集状态：{captureStatusLabels[current.status]}{current.errorCode ? ` · ${current.errorCode}` : ''}</p>}
            <Button type="button" disabled={readonly || saving || !dirtyKeys} onClick={() => void saveQuality()}>{saving ? '正在应用…' : '应用到设备'}</Button>
          </>}
        </FieldGroup>
      </TabsContent>
      <TabsContent value="text" className="pt-4"><FieldGroup>
        <Field><FieldLabel htmlFor="device-text">发送文本</FieldLabel><Input id="device-text" value={text} onChange={event => setText(event.currentTarget.value)} placeholder="输入或粘贴文本" disabled={readonly || commandBusy} /><FieldDescription>发送前请选中手机输入框</FieldDescription></Field>
        <Button type="button" disabled={readonly || commandBusy || !text} onClick={() => void command('text', { text }).then(reply => { if (reply.status === 'dispatched' || reply.payload?.applied === true) setText('') }).catch(reportCommandError)}><Keyboard data-icon="inline-start" />发送到设备</Button>
        <Button type="button" variant="outline" disabled={readonly || commandBusy} onClick={() => void command('clipboard.get', {}).then(reply => { const received = reply.payload?.text; if (typeof received === 'string') { setClipboardText(received); setClipboardRead(true) } }).catch(reportCommandError)}>读取手机剪贴板</Button>
        <Field>
          <FieldLabel htmlFor="clipboard-input">设备剪贴板文本</FieldLabel>
          <Input id="clipboard-input" value={clipboardText} onChange={event => setClipboardText(event.currentTarget.value)} disabled={readonly || commandBusy} />
          <div className="flex flex-wrap gap-2">
            <Button type="button" variant="outline" disabled={readonly || commandBusy} onClick={() => void command('clipboard.set', { text: clipboardText }).catch(reportCommandError)}>写入手机剪贴板</Button>
            <Button type="button" variant="outline" disabled={readonly || commandBusy || !clipboardText} onClick={() => void command('clipboard.paste', { text: clipboardText }).catch(reportCommandError)}>粘贴到设备</Button>
            {clipboardRead && <Button type="button" variant="outline" onClick={() => void copyPhoneClipboardToBrowser()}>复制文本到电脑剪贴板</Button>}
          </div>
          <FieldDescription>粘贴前先在设备画面中选中输入框；写入时留空可清空手机剪贴板。</FieldDescription>
        </Field>
      </FieldGroup></TabsContent>
      <TabsContent value="files" className="pt-4"><FieldGroup>
        <Field><FieldLabel>任务类型</FieldLabel><ToggleGroup variant="outline" value={[purpose]} onValueChange={value => { const next = value[0]; if (next === 'file' || next === 'apk') { setPurpose(next); setFile(null); if (fileInput.current) fileInput.current.value = ''; if (next === 'apk') setOverwrite(false) } }} disabled={transferLocked} aria-label="任务类型"><ToggleGroupItem value="file">普通文件</ToggleGroupItem><ToggleGroupItem value="apk">APK 安装</ToggleGroupItem></ToggleGroup></Field>
        <Field><FieldLabel htmlFor="device-file">文件或 APK</FieldLabel><div className="rounded-md border border-dashed p-2" onDragOver={event => { if (event.dataTransfer.types.includes('Files')) event.preventDefault() }} onDrop={event => { event.preventDefault(); if (transferLocked) return; const dropped = event.dataTransfer.files[0]; if (dropped) { setFile(dropped); if (fileInput.current) fileInput.current.value = '' } }}><Input ref={fileInput} id="device-file" type="file" accept={purpose === 'apk' ? '.apk,application/vnd.android.package-archive' : undefined} disabled={transferLocked} onChange={event => setFile(event.currentTarget.files?.[0] ?? null)} /><FieldDescription>也可将文件拖放到此处。{purpose === 'apk' ? '上传完成不代表安装成功。' : '普通文件默认发送到 Download。'}</FieldDescription></div></Field>
        {purpose === 'file' && <>
          <Field><FieldLabel htmlFor="target-path">目标路径（可选）</FieldLabel><Input id="target-path" value={targetPath} onChange={event => setTargetPath(event.currentTarget.value)} disabled={transferLocked} placeholder="默认：/sdcard/Download/" /></Field>
          <Field><FieldLabel>同名文件处理</FieldLabel><ToggleGroup variant="outline" value={[overwrite ? 'allow' : 'deny']} onValueChange={value => { if (value[0] === 'allow') setOverwrite(true); else if (value[0] === 'deny') setOverwrite(false) }} disabled={transferLocked} aria-label="覆盖同名文件"><ToggleGroupItem value="deny">禁止覆盖</ToggleGroupItem><ToggleGroupItem value="allow">明确允许覆盖</ToggleGroupItem></ToggleGroup></Field>
        </>}
        {file && <p className="text-xs text-muted-foreground">{file.name} · {file.size.toLocaleString()} 字节</p>}
        {transfer && <div className="rounded-md border p-3 text-sm" aria-live="polite">
          <p className="font-medium">{transfer.fileName} · {transferStateLabels[transfer.state]}</p>
          <p className="text-muted-foreground">已接收 {transfer.bytesReceived.toLocaleString()} / {transfer.size.toLocaleString()} 字节 · 已传输到设备 {transfer.bytesTransferred.toLocaleString()} / {transfer.size.toLocaleString()} 字节</p>
          {transfer.targetPath && <p className="break-all text-muted-foreground">目标：{transfer.targetPath}</p>}
          {transfer.result && <p>{transfer.result}</p>}{transfer.error && <p className="text-destructive">{transfer.error}</p>}
          {transfer.stdout && <pre className="max-h-24 overflow-auto whitespace-pre-wrap">{transfer.stdout}</pre>}{transfer.stderr && <pre className="max-h-24 overflow-auto whitespace-pre-wrap text-destructive">{transfer.stderr}</pre>}
        </div>}
        <div className="flex flex-wrap gap-2"><Button type="button" disabled={transferLocked || !file} onClick={() => void startTransfer()}><FileUp data-icon="inline-start" />{uploadBusy ? '正在上传…' : refreshBusy ? '正在读取状态…' : purpose === 'apk' ? '上传并安装 APK' : '上传文件'}</Button>
          {transfer && !terminalTransfer(transfer) && <Button type="button" variant="outline" disabled={cancelBusy} onClick={() => void cancelTransfer()}>{cancelBusy ? '正在取消…' : '取消后续传输'}</Button>}
          {transfer && <Button type="button" variant="outline" disabled={refreshBusy || cancelBusy} onClick={() => void refreshTransfer()}>{refreshBusy ? '正在刷新…' : '刷新结果'}</Button>}
        </div>
      </FieldGroup></TabsContent>
    </Tabs>
  )
}

function reportCommandError(error: unknown) {
  toast.add({ title: '设备操作未完成', description: errorMessage(error), type: 'error', timeout: 0 })
}
