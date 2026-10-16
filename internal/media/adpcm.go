package media

import (
	"bytes"
	"encoding/binary"
	"math"
)

// The marker lets updated clients decode both the new per-channel IMA ADPCM
// packets and packets sent by clients using the older custom ADPCM format.
var imaAudioMagic = [...]byte{'R', 'L', 'Y', 'A', 'U', 'D', '1', 0}

const imaAudioHeaderSize = 14

// EncodeADPCM encodes interleaved stereo float32 samples as IMA ADPCM. Each
// channel keeps an independent predictor and step index to avoid channel
// crosstalk and predictor overflow.
func EncodeADPCM(samples []float32) []byte {
	frames := len(samples) / audioPlaybackChannels
	if frames == 0 {
		return nil
	}
	samples = samples[:frames*audioPlaybackChannels]

	left := floatToPCM16(samples[0])
	right := floatToPCM16(samples[1])
	packet := make([]byte, imaAudioHeaderSize, imaAudioHeaderSize+frames-1)
	copy(packet, imaAudioMagic[:])
	binary.LittleEndian.PutUint16(packet[8:10], uint16(left))
	packet[10] = 0
	binary.LittleEndian.PutUint16(packet[11:13], uint16(right))
	packet[13] = 0

	leftPredictor, rightPredictor := int32(left), int32(right)
	leftIndex, rightIndex := 0, 0
	for frame := 1; frame < frames; frame++ {
		i := frame * audioPlaybackChannels
		leftCode, nextLeft, nextLeftIndex := encodeIMANibble(floatToPCM16(samples[i]), leftPredictor, leftIndex)
		rightCode, nextRight, nextRightIndex := encodeIMANibble(floatToPCM16(samples[i+1]), rightPredictor, rightIndex)
		packet = append(packet, leftCode<<4|rightCode)
		leftPredictor, leftIndex = nextLeft, nextLeftIndex
		rightPredictor, rightIndex = nextRight, nextRightIndex
	}
	return packet
}

// DecodeADPCM decodes the marked IMA format and retains support for packets
// sent by the previous custom ADPCM encoder.
func DecodeADPCM(data []byte) []float32 {
	if len(data) >= len(imaAudioMagic) && bytes.Equal(data[:len(imaAudioMagic)], imaAudioMagic[:]) {
		return decodeIMAADPCM(data)
	}
	return decodeADPCMLegacy(data)
}

func decodeIMAADPCM(data []byte) []float32 {
	if len(data) < imaAudioHeaderSize || int(data[10]) >= adpcmStepTableSize || int(data[13]) >= adpcmStepTableSize {
		return nil
	}
	leftPredictor := int32(int16(binary.LittleEndian.Uint16(data[8:10])))
	rightPredictor := int32(int16(binary.LittleEndian.Uint16(data[11:13])))
	leftIndex, rightIndex := int(data[10]), int(data[13])

	result := make([]float32, 2, 2+(len(data)-imaAudioHeaderSize)*2)
	result[0] = float32(leftPredictor) / 32768
	result[1] = float32(rightPredictor) / 32768
	for _, packed := range data[imaAudioHeaderSize:] {
		leftCode, rightCode := packed>>4, packed&0x0F
		leftPredictor, leftIndex = decodeIMANibble(leftCode, leftPredictor, leftIndex)
		rightPredictor, rightIndex = decodeIMANibble(rightCode, rightPredictor, rightIndex)
		result = append(result, float32(leftPredictor)/32768, float32(rightPredictor)/32768)
	}
	return result
}

func floatToPCM16(sample float32) int16 {
	if sample > 1 {
		sample = 1
	} else if sample < -1 {
		sample = -1
	}
	return int16(math.Round(float64(sample) * 32767))
}

func encodeIMANibble(sample int16, predictor int32, index int) (uint8, int32, int) {
	step := int32(adpcmStepTable[index])
	delta := int32(sample) - predictor
	code := uint8(0)
	if delta < 0 {
		code = 8
		delta = -delta
	}

	change := step >> 3
	if delta >= step {
		code |= 4
		delta -= step
		change += step
	}
	step >>= 1
	if delta >= step {
		code |= 2
		delta -= step
		change += step
	}
	step >>= 1
	if delta >= step {
		code |= 1
		change += step
	}

	if code&8 != 0 {
		predictor -= change
	} else {
		predictor += change
	}
	predictor = clampPCM16(predictor)
	index = clampADPCMIndex(index + int(adpcmIndexTable[code&7]))
	return code, predictor, index
}

func decodeIMANibble(code uint8, predictor int32, index int) (int32, int) {
	step := int32(adpcmStepTable[index])
	change := step >> 3
	if code&4 != 0 {
		change += step
	}
	if code&2 != 0 {
		change += step >> 1
	}
	if code&1 != 0 {
		change += step >> 2
	}
	if code&8 != 0 {
		predictor -= change
	} else {
		predictor += change
	}
	predictor = clampPCM16(predictor)
	index = clampADPCMIndex(index + int(adpcmIndexTable[code&7]))
	return predictor, index
}

func clampPCM16(sample int32) int32 {
	if sample > math.MaxInt16 {
		return math.MaxInt16
	}
	if sample < math.MinInt16 {
		return math.MinInt16
	}
	return sample
}

func clampADPCMIndex(index int) int {
	if index < 0 {
		return 0
	}
	if index >= adpcmStepTableSize {
		return adpcmStepTableSize - 1
	}
	return index
}
