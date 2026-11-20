import { HexViewer } from '@malsuke/react-hex-viewer'

export default function DeviceBinaryEditor({ bytes }: { bytes: string }) {
  const hex = Array.from(atob(bytes), byte => byte.charCodeAt(0).toString(16).padStart(2, '0')).join('')
  return <HexViewer hexString={hex} editable={false} showDebugPanel={false} fontFamily="monospace" />
}
