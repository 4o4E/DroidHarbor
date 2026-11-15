import assert from 'node:assert/strict'
import test from 'node:test'
import { stereoAudioOffer } from '../src/media/audioOffer.ts'

test('requests full-band stereo without modifying video or data channels', () => {
  const video = 'm=video 9 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 H264/90000\r\na=fmtp:111 packetization-mode=1\r\n'
  const audio = 'm=audio 9 UDP/TLS/RTP/SAVPF 109\r\na=rtpmap:109 opus/48000/2\r\na=fmtp:109 minptime=10;useinbandfec=1;stereo=0;maxplaybackrate=16000\r\n'
  const data = 'm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=sctp-port:5000\r\n'
  const result = stereoAudioOffer('v=0\r\n' + video + audio + data)
  assert.ok(result.includes(video))
  assert.ok(result.endsWith(data))
  const parameters = result.match(/^a=fmtp:109 (.*)\r?$/m)![1].trim().split(';')
  assert.deepEqual(parameters.sort(), ['minptime=10', 'useinbandfec=1', 'stereo=1', 'maxplaybackrate=48000'].sort())
  assert.equal(stereoAudioOffer(result), result)
})

test('adds stereo parameters when an Opus offer has no fmtp line', () => {
  const result = stereoAudioOffer('v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\na=recvonly\r\n')
  assert.ok(result.includes('a=rtpmap:111 opus/48000/2\r\na=fmtp:111 stereo=1;maxplaybackrate=48000\r\n'))
  assert.ok(result.endsWith('a=recvonly\r\n'))
})
