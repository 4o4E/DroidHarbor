// opus/48000/2 alone still defaults to mono. The receiver must request stereo
// in SDP; setCodecPreferences only accepts the browser's existing capabilities.
export function stereoAudioOffer(sdp: string): string {
  const newline = sdp.includes('\r\n') ? '\r\n' : '\n'
  return sdp.split(/(?=^m=)/m).map(section => {
    if (!section.startsWith('m=audio ')) return section
    for (const match of section.matchAll(/^a=rtpmap:(\d+) opus\/48000\/2(?=\r?$)/gmi)) {
      const pattern = new RegExp(`^a=fmtp:${match[1]} ([^\\r\\n]*)`, 'm')
      const previous = pattern.exec(section)
      const parameters = (previous?.[1] ?? '').split(';').map(value => value.trim())
        .filter(value => value && !/^(stereo|maxplaybackrate)\s*=/i.test(value))
      const fmtp = `a=fmtp:${match[1]} ${[...parameters, 'stereo=1', 'maxplaybackrate=48000'].join(';')}`
      section = previous ? section.replace(pattern, fmtp) : section.replace(match[0], `${match[0]}${newline}${fmtp}`)
    }
    return section
  }).join('')
}
