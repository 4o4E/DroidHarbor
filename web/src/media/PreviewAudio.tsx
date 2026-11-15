import { useCallback, useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Volume2, VolumeX, Smartphone } from 'lucide-react'
import { Select as SelectPrimitive } from '@base-ui/react/select'
import { Button } from '@/components/ui/button'
import { Select, SelectContent, SelectGroup, SelectItem, SelectValue } from '@/components/ui/select'
import { Slider } from '@/components/ui/slider'
import { toast } from '@/components/ui/toast'
import { api, errorMessage } from '@/lib/api'
import { useSystemPreferences } from '@/lib/queries'
import { useErrorNotification } from '@/lib/notifications'
import type { AudioRoute, SessionMode } from '@/lib/types'
import type { MediaSession } from './types'
import { audioItems, type AudioSnapshot } from './audio'
import { setWebAudioGain } from './audioOutput'

type DeviceVolume = { current: number; min: number; max: number }
type AudioGraph = { context: AudioContext; gain: GainNode }

export function PreviewAudio({ session, mode }: { session: MediaSession; mode: SessionMode }) {
  const id = session.sessionId
  const client = useQueryClient()
  const preferences = useSystemPreferences()
  const graph = useRef<AudioGraph | null>(null)
  const changing = useRef(false)
  const volumeChanging = useRef(false)
  const [saving, setSaving] = useState(false)
  const [volumeSaving, setVolumeSaving] = useState(false)
  const [volumeDraft, setVolumeDraft] = useState<number | null>(null)
  const [browserVolume, setBrowserVolume] = useState(100)
  const [enhancement, setEnhancement] = useState(1)
  const levels = useRef({ volume: 100, enhancement: 1 })
  const [visible, setVisible] = useState(document.visibilityState === 'visible')
  const [playError, setPlayError] = useState('')
  const query = useQuery({ queryKey: ['session-audio', id], enabled: !!id && session.status === 'connected', queryFn: ({ signal }) => api<AudioSnapshot>(`/sessions/${encodeURIComponent(id!)}/audio`, { signal }), refetchInterval: 1000, refetchIntervalInBackground: false })
  const route = query.data
  const onDevice = route?.route === 'device'
  const onBrowser = route?.route === 'browser'
  const volumeQuery = useQuery({ queryKey: ['session-volume', id], enabled: !!id && session.ready && onDevice && route?.status === 'ready', queryFn: ({ signal }) => api<DeviceVolume>(`/sessions/${encodeURIComponent(id!)}/volume`, { signal }), refetchInterval: volumeSaving ? false : 2000, refetchIntervalInBackground: false })
  const maxGain = preferences.data?.audioGainMax
  const effectiveGain = Math.min(enhancement, maxGain ?? 1)
  useErrorNotification('无法读取增强设置', preferences.error)
  useErrorNotification('无法读取音频状态', query.error)
  useErrorNotification('无法读取手机媒体音量', onDevice ? volumeQuery.error : null)
  useErrorNotification('媒体音频异常', route?.error)
  useErrorNotification('网页音频未播放', playError)
  const audible = !!id && onBrowser && route.status === 'ready' && route.ownerSessionId === id && session.ready && visible

  const output = useCallback(() => {
    if (!graph.current) {
      const context = new AudioContext()
      const gain = context.createGain()
      gain.gain.value = 0
      gain.connect(context.destination)
      graph.current = { context, gain }
    }
    return graph.current
  }, [])

  useEffect(() => {
    const changed = () => setVisible(document.visibilityState === 'visible')
    document.addEventListener('visibilitychange', changed)
    return () => document.removeEventListener('visibilitychange', changed)
  }, [])
  useEffect(() => setVolumeDraft(null), [id, onDevice])
  useEffect(() => () => {
    const current = graph.current
    graph.current = null
    if (current) {
      current.gain.disconnect()
      void current.context.close().catch(error => toast.add({ title: '网页音频清理失败', description: errorMessage(error), type: 'error', timeout: 0 }))
    }
  }, [])
  useEffect(() => {
    if (maxGain !== undefined && enhancement > maxGain) {
      setEnhancement(maxGain)
      toast.add({ title: '音频增强已调整', description: `系统增强上限已改为 ${maxGain}×，当前增强已降低至此上限。`, type: 'warning', timeout: 0 })
    }
  }, [maxGain, enhancement])
  useEffect(() => {
    levels.current = { volume: browserVolume, enhancement: effectiveGain }
    if (graph.current) setWebAudioGain(graph.current.gain, browserVolume, effectiveGain)
  }, [browserVolume, effectiveGain])
  useEffect(() => {
    if (!audible || !session.audioStream) return
    let source: MediaStreamAudioSourceNode | undefined
    let active = true
    const tracks = session.audioStream.getAudioTracks()
    const ended = () => { if (active) setPlayError('网页音频轨道已结束，请使用“重新连接视频”恢复。') }
    for (const track of tracks) track.addEventListener('ended', ended)
    try {
      const current = output()
      source = current.context.createMediaStreamSource(session.audioStream)
      source.connect(current.gain)
      setWebAudioGain(current.gain, levels.current.volume, levels.current.enhancement)
      if (current.context.state === 'suspended' && !navigator.userActivation.hasBeenActive) {
        setPlayError('浏览器尚未允许播放声音；点击“网页播放”启用。')
      } else {
        void current.context.resume().then(() => { if (active) setPlayError('') }).catch(error => { if (active) setPlayError(`${errorMessage(error)}；点击“网页播放”重试。`) })
      }
    } catch (error) { setPlayError(errorMessage(error)) }
    return () => { active = false; for (const track of tracks) track.removeEventListener('ended', ended); source?.disconnect() }
  }, [audible, session.audioStream, output])

  const change = async (value: AudioRoute) => {
    if (!id || !route || changing.current) return
    changing.current = true
    setSaving(true)
    setVolumeDraft(null)
    try {
      // Resume in the menu click's user activation, before any network await.
      if (value === 'browser') {
        if (!session.audioStream?.getAudioTracks().some(track => track.readyState === 'live')) throw new Error('本窗口没有可用音频轨道，请使用“重新连接视频”恢复。')
        await output().context.resume()
      }
      const snapshot = await api<AudioSnapshot>(`/sessions/${encodeURIComponent(id)}/audio`, { method: 'PATCH', body: { route: value, revision: route.revision } })
      client.setQueryData(['session-audio', id], snapshot)
      await client.invalidateQueries({ queryKey: ['session-audio'] })
      setPlayError('')
    } catch (error) { toast.add({ title: '音频切换未完成', description: errorMessage(error), type: 'error', timeout: 0 }); await client.invalidateQueries({ queryKey: ['session-audio'] }) }
    finally { changing.current = false; setSaving(false) }
  }
  const commitVolume = async (value: number) => {
    if (!id || !onDevice || volumeChanging.current) return
    volumeChanging.current = true
    setVolumeSaving(true)
    try {
      const result = await api<DeviceVolume>(`/sessions/${encodeURIComponent(id)}/volume`, { method: 'PATCH', body: { level: value } })
      client.setQueryData(['session-volume', id], result)
    } catch (error) { toast.add({ title: '手机音量未确认', description: errorMessage(error), type: 'error', timeout: 0 }) }
    finally {
      setVolumeDraft(null)
      volumeChanging.current = false
      setVolumeSaving(false)
      await client.invalidateQueries({ queryKey: ['session-volume', id] })
    }
  }
  const volume = onDevice ? volumeDraft ?? volumeQuery.data?.current ?? 0 : browserVolume
  const volumeDisabled = !session.ready || saving || volumeSaving || route?.status !== 'ready' || (!onBrowser && !onDevice) || (onDevice && (mode !== 'control' || !volumeQuery.data))
  const Icon = route?.status === 'failed' || route?.route === 'muted' ? VolumeX : onDevice ? Smartphone : Volume2
  return <div className="preview-audio">
    <div className="preview-volume-anchor">
      <div className="preview-volume-panel" role="group" aria-label="音量与增强">
        <div className="preview-volume">
          <Slider orientation="vertical" aria-label={onDevice ? '设备媒体音量' : '网页音量'} value={[volume]} min={onDevice ? volumeQuery.data?.min ?? 0 : 0} max={onDevice ? volumeQuery.data?.max ?? 100 : 100} step={1} disabled={volumeDisabled} onValueChange={value => { const next = Array.isArray(value) ? value[0] : value; if (onBrowser) setBrowserVolume(next); else setVolumeDraft(next) }} onValueCommitted={value => { if (onDevice) void commitVolume(Array.isArray(value) ? value[0] : value) }} />
          <output>{onDevice ? volumeQuery.data ? `${volume}/${volumeQuery.data.max}` : '—' : onBrowser ? `${volume}%` : '—'}</output>
        </div>
        <div className="preview-volume preview-enhancement" title="网页增强">
          <Slider orientation="vertical" aria-label="网页音频增强" value={[effectiveGain]} min={0} max={maxGain ?? 1} step={0.1} disabled={!onBrowser || !session.ready || maxGain === undefined} onValueChange={value => setEnhancement(Array.isArray(value) ? value[0] : value)} />
          <output>{effectiveGain.toFixed(1)}×</output>
        </div>
      </div>
    </div>
    <Select items={audioItems} value={route?.status === 'failed' ? null : route?.target || null} disabled={!route || !id || mode !== 'control' || saving || volumeSaving || session.status !== 'connected'} onValueChange={value => { if (value) void change(value as AudioRoute) }}>
      <SelectPrimitive.Trigger render={<Button size="icon-sm" variant="ghost" aria-label="音频播放位置" title={route?.status === 'failed' ? '音频异常' : `音频播放位置：${audioItems.find(item => item.value === route?.target)?.label ?? '读取中'}`} />}><Icon /><SelectValue className="sr-only" placeholder="音频播放位置" /></SelectPrimitive.Trigger>
      <SelectContent side="right" align="end" alignItemWithTrigger={false}><SelectGroup>{audioItems.map(item => <SelectItem value={item.value} key={item.value} onClick={() => { if (route?.target === item.value && item.value === 'browser') void change('browser') }}>{item.label}</SelectItem>)}</SelectGroup></SelectContent>
    </Select>
  </div>
}
