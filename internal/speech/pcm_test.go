package speech

import (
	"encoding/binary"
	"testing"
	"time"
)

func stereo(samples ...int16) []byte {
	pcm := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(sample))
	}
	return pcm
}

func samples(pcm []byte) []int16 {
	values := make([]int16, len(pcm)/2)
	for index := range values {
		values[index] = int16(binary.LittleEndian.Uint16(pcm[index*2:]))
	}
	return values
}

func TestResample(t *testing.T) {
	pcm := stereo(0, 100, 1000, -100, 2000, -32768, 3000, 32767)
	if got := resample(pcm, 22050, 22050); &got[0] != &pcm[0] {
		t.Fatal("same-rate PCM was copied")
	}
	up := samples(resample(pcm, 22050, 44100))
	want := []int16{0, 100, 500, 0, 1000, -100, 1500, -16434, 2000, -32768, 2500, -1, 3000, 32767, 3000, 32767}
	if len(up) != len(want) {
		t.Fatalf("upsampled %d samples", len(up))
	}
	for index := range want {
		if up[index] != want[index] {
			t.Fatalf("upsampled %v, want %v", up, want)
		}
	}
	down := samples(resample(pcm, 44100, 22050))
	if len(down) != 4 || down[0] != 0 || down[1] != 100 || down[2] != 2000 || down[3] != -32768 {
		t.Fatalf("downsampled %v", down)
	}
	if got := resample(stereo(7, 8), 48000, 8000); len(samples(got)) != 2 {
		t.Fatalf("short clip lost: %v", samples(got))
	}
	if got := resample(nil, 22050, 44100); len(got) != 0 {
		t.Fatal("empty PCM grew")
	}
}

func TestPCMDuration(t *testing.T) {
	if got := pcmDuration(make([]byte, 22050*bytesPerFrame), 22050); got != time.Second {
		t.Fatalf("duration %s", got)
	}
	if got := pcmDuration(make([]byte, bytesPerFrame), 0); got != 0 {
		t.Fatalf("zero-rate duration %s", got)
	}
}
