package media

import "encoding/binary"

// SilentWAV is milliseconds of silence as a 16 kHz mono 16-bit PCM WAV file:
// the smallest audio a transcription certification can send.
func SilentWAV(milliseconds int) []byte {
	return wavFromPCM(make([]byte, 16000*milliseconds/1000*2), 16000)
}

// wavFromPCM wraps mono 16-bit little-endian PCM at rate in a WAV header.
func wavFromPCM(pcm []byte, rate int) []byte {
	const bytesPerSample = 2
	data := len(pcm)
	wav := make([]byte, 44, 44+data)
	copy(wav[0:], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(36+data))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1) // PCM
	binary.LittleEndian.PutUint16(wav[22:], 1) // mono
	binary.LittleEndian.PutUint32(wav[24:], uint32(rate))
	binary.LittleEndian.PutUint32(wav[28:], uint32(rate*bytesPerSample))
	binary.LittleEndian.PutUint16(wav[32:], bytesPerSample)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(data))
	return append(wav, pcm...)
}
