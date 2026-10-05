package media

import "encoding/binary"

// SilentWAV is milliseconds of silence as a 16 kHz mono 16-bit PCM WAV file:
// the smallest audio a transcription certification can send.
func SilentWAV(milliseconds int) []byte {
	const rate, bytesPerSample = 16000, 2
	samples := rate * milliseconds / 1000
	data := samples * bytesPerSample
	wav := make([]byte, 44+data)
	copy(wav[0:], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(36+data))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1) // PCM
	binary.LittleEndian.PutUint16(wav[22:], 1) // mono
	binary.LittleEndian.PutUint32(wav[24:], rate)
	binary.LittleEndian.PutUint32(wav[28:], rate*bytesPerSample)
	binary.LittleEndian.PutUint16(wav[32:], bytesPerSample)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(data))
	return wav
}
