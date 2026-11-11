import { useQuery } from '@tanstack/react-query'
import { apiResponse } from '@/lib/api'

export type DevicePreviewFrame = {
  blob: Blob
  capturedAt: string
  linkGeneration: string
}

export function useDevicePreview(deviceId: string, linkSignature: string, enabled: boolean) {
  return useQuery({
    queryKey: ['device-preview', deviceId, linkSignature],
    enabled,
    queryFn: async ({ signal }): Promise<DevicePreviewFrame> => {
      const response = await apiResponse(`/devices/${encodeURIComponent(deviceId)}/preview`, { signal })

      const capturedAt = response.headers.get('X-DroidHarbor-Captured-At')
      const linkGeneration = response.headers.get('X-DroidHarbor-Link-Generation')
      if (response.headers.get('X-DroidHarbor-Device-ID') !== deviceId) {
        throw new Error('预览响应设备不匹配')
      }
      if (!capturedAt || !Number.isFinite(Date.parse(capturedAt)) || !linkGeneration || !/^\d+$/.test(linkGeneration)) {
        throw new Error('预览响应缺少有效的拍摄时间或链路代次')
      }
      if (!response.headers.get('Content-Type')?.toLowerCase().startsWith('image/jpeg')) {
        throw new Error('预览响应不是 JPEG 图片')
      }

      const blob = await response.blob()
      if (blob.size === 0) throw new Error('预览图片为空')
      return { blob, capturedAt, linkGeneration }
    },
    staleTime: 0,
    gcTime: 30_000,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    refetchInterval: 5_000,
    refetchIntervalInBackground: false,
  })
}
