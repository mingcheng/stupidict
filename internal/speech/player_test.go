package speech

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	dictionary "github.com/mingcheng/stupidict"
)

func silentMP3() []byte {
	frame := make([]byte, 417)
	copy(frame, []byte{0xff, 0xfb, 0x90, 0x00})
	return bytes.Repeat(frame, 3)
}

func TestDecodeAndPlay(t *testing.T) {
	player := New()
	called := false
	player.output = func(_ context.Context, pcm []byte, sampleRate int) error {
		called = true
		if sampleRate != 44100 || len(pcm) != 3*1152*4 || !bytes.Equal(pcm, make([]byte, len(pcm))) {
			t.Errorf("decoded audio: %d Hz, %d bytes", sampleRate, len(pcm))
		}
		return nil
	}
	if err := player.Play(context.Background(), silentMP3(), "cat", dictionary.American); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("decoded audio was not played")
	}
}

func TestInvalidAudioAndDeviceErrors(t *testing.T) {
	player := New()
	deviceError := errors.New("device unavailable")
	calls := 0
	player.output = func(context.Context, []byte, int) error { calls++; return deviceError }
	for _, test := range []struct {
		name   string
		audio  []byte
		accent dictionary.Accent
	}{
		{"missing", nil, dictionary.British},
		{"corrupt", []byte("not MP3"), dictionary.American},
		{"invalid accent", silentMP3(), dictionary.Accent("invalid")},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := player.Play(context.Background(), test.audio, "cat", test.accent)
			if err == nil {
				t.Fatal("invalid input succeeded")
			}
			if test.audio == nil && !errors.Is(err, dictionary.ErrAudioNotFound) {
				t.Fatalf("missing audio: %v", err)
			}
		})
	}
	if calls != 0 {
		t.Fatal("invalid audio opened a device")
	}
	if err := player.Play(context.Background(), silentMP3(), "cat", dictionary.British); !errors.Is(err, deviceError) {
		t.Fatalf("device error lost: %v", err)
	}
}

func TestStopAndReplacement(t *testing.T) {
	player := New()
	started := make(chan struct{}, 2)
	var active atomic.Int32
	player.output = func(ctx context.Context, _ []byte, _ int) error {
		if active.Add(1) != 1 {
			t.Error("playback devices overlap")
		}
		defer active.Add(-1)
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	waitStarted := func() {
		t.Helper()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("playback did not start")
		}
	}
	waitCanceled := func(done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("playback did not stop")
		}
	}
	first := make(chan error, 1)
	go func() { first <- player.Play(context.Background(), silentMP3(), "cat", dictionary.British) }()
	t.Cleanup(player.Stop)
	waitStarted()
	second := make(chan error, 1)
	go func() { second <- player.Play(context.Background(), silentMP3(), "cat", dictionary.American) }()
	waitCanceled(first)
	waitStarted()
	player.Stop()
	player.Stop()
	waitCanceled(second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := player.Play(ctx, silentMP3(), "cat", dictionary.British); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled: %v", err)
	}
	player.output = func(context.Context, []byte, int) error { return nil }
	if err := player.Play(context.Background(), silentMP3(), "cat", dictionary.British); err != nil {
		t.Fatalf("reuse after stop: %v", err)
	}
}

func TestRealAudioDecode(t *testing.T) {
	path := os.Getenv("STUPIDDICT_AUDIO")
	if path == "" {
		t.Skip("STUPIDDICT_AUDIO is not set")
	}
	store, err := dictionary.OpenAudio(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, accent := range []dictionary.Accent{dictionary.British, dictionary.American} {
		audio, err := store.GetAudio(context.Background(), "cat", accent)
		if err != nil {
			t.Fatal(err)
		}
		pcm, rate, err := decodeMP3(context.Background(), audio)
		if err != nil || len(pcm) == 0 || rate <= 0 {
			t.Fatalf("%s: %d bytes, %d Hz, %v", accent, len(pcm), rate, err)
		}
	}
}
