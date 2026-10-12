package speech

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// device is the process-wide audio output. Backends such as Oto allow a single
// device per process, so it is opened lazily on first playback and then reused.
type device interface {
	SampleRate() int
	// Ready closes when initialization finishes; Err then reports failures.
	Ready() <-chan struct{}
	NewVoice(io.Reader) voice
	Suspend() error
	Resume() error
	// Err reports a fatal device error; the device is unusable afterwards.
	Err() error
}

// voice is one sound playing on a device.
type voice interface {
	Play()
	Pause()
	IsPlaying() bool
	Err() error
	Close() error
}

// deviceOutput plays interleaved signed 16-bit little-endian stereo PCM on a
// shared device, one clip at a time.
type deviceOutput struct {
	open func(sampleRate int) (device, error)
	// linger keeps the device running after playback so buffered audio drains
	// before it is suspended; an idle device would otherwise keep the system
	// audio path awake.
	linger      time.Duration
	poll        time.Duration
	initTimeout time.Duration

	gate       sync.Mutex
	device     device
	err        error
	generation uint64
}

var native = &deviceOutput{open: openOto, linger: time.Second, poll: 10 * time.Millisecond, initTimeout: 10 * time.Second}

func playPCM(ctx context.Context, pcm []byte, sampleRate int) error {
	return native.play(ctx, pcm, sampleRate)
}

func (output *deviceOutput) play(ctx context.Context, pcm []byte, sampleRate int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	output.gate.Lock()
	defer output.gate.Unlock()
	device, err := output.acquire(ctx, sampleRate)
	if err != nil {
		return err
	}
	output.generation++
	defer output.suspendLater(device)
	if err := device.Resume(); err != nil {
		return fmt.Errorf("resume native audio: %w", err)
	}
	pcm = resample(pcm, sampleRate, device.SampleRate())
	voice := device.NewVoice(bytes.NewReader(pcm))
	defer voice.Close()
	voice.Play()
	deadline := time.NewTimer(pcmDuration(pcm, device.SampleRate()) + 5*time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(output.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			voice.Pause()
			return ctx.Err()
		case <-deadline.C:
			voice.Pause()
			return errors.New("native audio playback timed out")
		case <-ticker.C:
		}
		if err := device.Err(); err != nil {
			return unavailable(err)
		}
		if err := voice.Err(); err != nil {
			return fmt.Errorf("native audio playback: %w", err)
		}
		if !voice.IsPlaying() {
			return nil
		}
	}
}

// acquire opens the device once. Opening cannot be retried in-process, so a
// failure is kept and reported by every later playback.
func (output *deviceOutput) acquire(ctx context.Context, sampleRate int) (device, error) {
	if output.device == nil && output.err == nil {
		output.device, output.err = output.open(sampleRate)
		if output.err != nil {
			output.err = unavailable(output.err)
		}
	}
	if output.err != nil {
		return nil, output.err
	}
	timeout := time.NewTimer(output.initTimeout)
	defer timeout.Stop()
	select {
	case <-output.device.Ready():
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout.C:
		return nil, errors.New("native audio initialization timed out")
	}
	if err := output.device.Err(); err != nil {
		return nil, unavailable(err)
	}
	return output.device, nil
}

// suspendLater suspends the device after the linger period unless another
// playback starts first. The caller must hold output.gate.
func (output *deviceOutput) suspendLater(device device) {
	generation := output.generation
	time.AfterFunc(output.linger, func() {
		output.gate.Lock()
		defer output.gate.Unlock()
		if output.generation == generation {
			_ = device.Suspend()
		}
	})
}

func unavailable(err error) error {
	return fmt.Errorf("native audio unavailable (restart stupidict to retry): %w", err)
}
