package speech

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDevice struct {
	gate                  sync.Mutex
	sampleRate            int
	ready                 chan struct{}
	err                   error
	polls                 int
	voices                []*fakeVoice
	suspended             bool
	suspends, resumes     int
	suspendedWhilePlaying bool
}

func newFakeDevice(sampleRate, polls int) *fakeDevice {
	ready := make(chan struct{})
	close(ready)
	return &fakeDevice{sampleRate: sampleRate, ready: ready, polls: polls, suspended: true}
}

func (device *fakeDevice) SampleRate() int        { return device.sampleRate }
func (device *fakeDevice) Ready() <-chan struct{} { return device.ready }

func (device *fakeDevice) NewVoice(reader io.Reader) voice {
	data, _ := io.ReadAll(reader)
	device.gate.Lock()
	defer device.gate.Unlock()
	voice := &fakeVoice{device: device, data: data, remaining: device.polls}
	device.voices = append(device.voices, voice)
	return voice
}

func (device *fakeDevice) Suspend() error {
	device.gate.Lock()
	defer device.gate.Unlock()
	for _, voice := range device.voices {
		if voice.playing {
			device.suspendedWhilePlaying = true
		}
	}
	device.suspended = true
	device.suspends++
	return nil
}

func (device *fakeDevice) Resume() error {
	device.gate.Lock()
	defer device.gate.Unlock()
	device.suspended = false
	device.resumes++
	return nil
}

func (device *fakeDevice) Err() error {
	device.gate.Lock()
	defer device.gate.Unlock()
	return device.err
}

func (device *fakeDevice) state() (suspends, resumes int, suspended, overlapped bool) {
	device.gate.Lock()
	defer device.gate.Unlock()
	return device.suspends, device.resumes, device.suspended, device.suspendedWhilePlaying
}

// fakeVoice finishes after a number of IsPlaying polls; -1 plays forever.
type fakeVoice struct {
	device                  *fakeDevice
	data                    []byte
	remaining               int
	playing, paused, closed bool
}

func (voice *fakeVoice) Play() {
	voice.device.gate.Lock()
	voice.playing = true
	voice.device.gate.Unlock()
}
func (voice *fakeVoice) Err() error { return nil }

func (voice *fakeVoice) Pause() {
	voice.device.gate.Lock()
	defer voice.device.gate.Unlock()
	voice.playing, voice.paused = false, true
}

func (voice *fakeVoice) IsPlaying() bool {
	voice.device.gate.Lock()
	defer voice.device.gate.Unlock()
	if voice.playing && voice.remaining == 0 {
		voice.playing = false
	} else if voice.remaining > 0 {
		voice.remaining--
	}
	return voice.playing
}

func (voice *fakeVoice) Close() error {
	voice.device.gate.Lock()
	defer voice.device.gate.Unlock()
	voice.playing, voice.closed = false, true
	return nil
}

func testOutput(open func(int) (device, error)) *deviceOutput {
	return &deviceOutput{open: open, linger: 30 * time.Millisecond, poll: time.Millisecond, initTimeout: time.Second}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDeviceOutputPlaysAndSuspendsWhenIdle(t *testing.T) {
	fake := newFakeDevice(22050, 3)
	opens := 0
	output := testOutput(func(sampleRate int) (device, error) {
		opens++
		if sampleRate != 22050 {
			t.Errorf("device opened at %d Hz", sampleRate)
		}
		return fake, nil
	})
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	if err := output.play(context.Background(), pcm, 22050); err != nil {
		t.Fatal(err)
	}
	if voice := fake.voices[0]; string(voice.data) != string(pcm) || !voice.closed || voice.paused {
		t.Fatalf("voice: %+v", voice)
	}
	waitFor(t, func() bool { suspends, _, suspended, _ := fake.state(); return suspended && suspends == 1 })
	if err := output.play(context.Background(), make([]byte, 8*bytesPerFrame), 44100); err != nil {
		t.Fatal(err)
	}
	if opens != 1 {
		t.Fatalf("device opened %d times", opens)
	}
	if got := len(fake.voices[1].data); got != 4*bytesPerFrame {
		t.Fatalf("44.1 kHz clip not resampled to device rate: %d bytes", got)
	}
	if _, resumes, _, _ := fake.state(); resumes != 2 {
		t.Fatalf("resumed %d times", resumes)
	}
}

func TestDeviceOutputKeepsRunningBetweenClips(t *testing.T) {
	fake := newFakeDevice(22050, 0)
	output := testOutput(func(int) (device, error) { return fake, nil })
	output.linger = 50 * time.Millisecond
	if err := output.play(context.Background(), make([]byte, bytesPerFrame), 22050); err != nil {
		t.Fatal(err)
	}
	fake.polls = 80
	if err := output.play(context.Background(), make([]byte, bytesPerFrame), 22050); err != nil {
		t.Fatal(err)
	}
	if suspends, _, _, overlapped := fake.state(); overlapped || suspends != 0 {
		t.Fatalf("stale idle timer suspended playback: suspends=%d overlapped=%v", suspends, overlapped)
	}
	waitFor(t, func() bool { suspends, _, _, _ := fake.state(); return suspends == 1 })
}

func TestDeviceOutputCancellation(t *testing.T) {
	fake := newFakeDevice(22050, -1)
	output := testOutput(func(int) (device, error) { return fake, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := output.play(ctx, make([]byte, 22050*bytesPerFrame), 22050); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation: %v", err)
	}
	if voice := fake.voices[0]; !voice.paused || !voice.closed {
		t.Fatalf("canceled voice: %+v", voice)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := output.play(canceled, make([]byte, bytesPerFrame), 22050); !errors.Is(err, context.Canceled) || len(fake.voices) != 1 {
		t.Fatalf("already canceled: %v", err)
	}
}

func TestDeviceOutputInitializationFailureIsKept(t *testing.T) {
	cause := errors.New("no audio server")
	opens := 0
	output := testOutput(func(int) (device, error) { opens++; return nil, cause })
	for range 2 {
		err := output.play(context.Background(), make([]byte, bytesPerFrame), 22050)
		if !errors.Is(err, cause) || !strings.Contains(err.Error(), "restart") {
			t.Fatalf("initialization error: %v", err)
		}
	}
	if opens != 1 {
		t.Fatalf("device opened %d times", opens)
	}
}

func TestDeviceOutputReportsDeviceFailures(t *testing.T) {
	cause := errors.New("device lost")
	pending := newFakeDevice(22050, -1)
	pending.ready = make(chan struct{})
	output := testOutput(func(int) (device, error) { return pending, nil })
	output.initTimeout = 10 * time.Millisecond
	if err := output.play(context.Background(), make([]byte, bytesPerFrame), 22050); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("pending initialization: %v", err)
	}
	close(pending.ready)
	done := make(chan error, 1)
	go func() { done <- output.play(context.Background(), make([]byte, 22050*bytesPerFrame), 22050) }()
	waitFor(t, func() bool { pending.gate.Lock(); defer pending.gate.Unlock(); return len(pending.voices) == 1 })
	pending.gate.Lock()
	pending.err = cause
	pending.gate.Unlock()
	if err := <-done; !errors.Is(err, cause) {
		t.Fatalf("device failure during playback: %v", err)
	}
	if err := output.play(context.Background(), make([]byte, bytesPerFrame), 22050); !errors.Is(err, cause) {
		t.Fatalf("device failure before playback: %v", err)
	}
}

// TestNativeDevicePlayback opens the real audio device and plays silence.
func TestNativeDevicePlayback(t *testing.T) {
	if os.Getenv("STUPIDDICT_PLAYBACK") == "" {
		t.Skip("STUPIDDICT_PLAYBACK is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := playPCM(ctx, make([]byte, 22050/5*bytesPerFrame), 22050); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if err := playPCM(short, make([]byte, 22050*bytesPerFrame), 22050); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("native cancellation: %v", err)
	}
}
