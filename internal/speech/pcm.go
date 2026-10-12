package speech

import (
	"encoding/binary"
	"math"
	"time"
)

// bytesPerFrame is the size of one interleaved signed 16-bit stereo frame.
const bytesPerFrame = 4

// resample converts PCM between sample rates with linear interpolation. The
// audio pack uses a single rate, so this only adapts unexpected clips to the
// rate the shared device was opened with.
func resample(pcm []byte, from, to int) []byte {
	frames := len(pcm) / bytesPerFrame
	if from == to || from <= 0 || to <= 0 || frames == 0 {
		return pcm
	}
	outFrames := max(1, int(int64(frames)*int64(to)/int64(from)))
	out := make([]byte, outFrames*bytesPerFrame)
	sample := func(frame, channel int) float64 {
		return float64(int16(binary.LittleEndian.Uint16(pcm[frame*bytesPerFrame+channel*2:])))
	}
	for frame := range outFrames {
		position := float64(frame) * float64(from) / float64(to)
		left := min(int(position), frames-1)
		right := min(left+1, frames-1)
		weight := position - float64(left)
		for channel := range 2 {
			a, b := sample(left, channel), sample(right, channel)
			value := int16(math.Round(a + (b-a)*weight))
			binary.LittleEndian.PutUint16(out[frame*bytesPerFrame+channel*2:], uint16(value))
		}
	}
	return out
}

func pcmDuration(pcm []byte, sampleRate int) time.Duration {
	if sampleRate <= 0 {
		return 0
	}
	return time.Duration(len(pcm)/bytesPerFrame) * time.Second / time.Duration(sampleRate)
}
