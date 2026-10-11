//go:build cgo

package speech

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
)

func playPCM(ctx context.Context, pcm []byte, sampleRate int) error {
	var backends []malgo.Backend
	switch runtime.GOOS {
	case "windows":
		backends = []malgo.Backend{malgo.BackendWasapi, malgo.BackendDsound, malgo.BackendWinmm}
	case "darwin":
		backends = []malgo.Backend{malgo.BackendCoreaudio}
	default:
		backends = []malgo.Backend{malgo.BackendPulseaudio, malgo.BackendAlsa, malgo.BackendJack}
	}
	return playNative(ctx, pcm, sampleRate, backends)
}

func playNative(ctx context.Context, pcm []byte, sampleRate int, backends []malgo.Backend) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	audioContext, err := malgo.InitContext(backends, malgo.ContextConfig{}, nil)
	if err != nil {
		return fmt.Errorf("initialize native audio: %w", err)
	}
	defer func() {
		_ = audioContext.Uninit()
		audioContext.Free()
	}()
	config := malgo.DefaultDeviceConfig(malgo.Playback)
	config.Playback.Format = malgo.FormatS16
	config.Playback.Channels = 2
	config.SampleRate = uint32(sampleRate)
	config.PeriodSizeInMilliseconds = 10
	config.Periods = 2
	config.Alsa.NoMMap = 1
	stream := pcmStream{data: pcm, drainBytes: sampleRate * 4 / 5, done: make(chan struct{})}
	stopped := make(chan struct{})
	var stopOnce sync.Once
	device, err := malgo.InitDevice(audioContext.Context, config, malgo.DeviceCallbacks{
		Data: func(output, _ []byte, _ uint32) { stream.fill(output) },
		Stop: func() { stopOnce.Do(func() { close(stopped) }) },
	})
	if err != nil {
		return fmt.Errorf("open native audio device: %w", err)
	}
	defer device.Uninit()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := device.Start(); err != nil {
		return fmt.Errorf("start native audio: %w", err)
	}
	duration := time.Duration(len(pcm)/4) * time.Second / time.Duration(sampleRate)
	deadline := time.NewTimer(duration + 5*time.Second)
	defer deadline.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-stream.done:
		return ctx.Err()
	case <-stopped:
		return errors.New("native audio device stopped unexpectedly")
	case <-deadline.C:
		return errors.New("native audio playback timed out")
	}
}

type pcmStream struct {
	data       []byte
	drainBytes int
	done       chan struct{}
	finished   bool
}

func (stream *pcmStream) fill(output []byte) {
	clear(output)
	if len(stream.data) > 0 {
		written := copy(output, stream.data)
		stream.data = stream.data[written:]
		return
	}
	stream.drainBytes -= len(output)
	if stream.drainBytes <= 0 && !stream.finished {
		stream.finished = true
		close(stream.done)
	}
}
