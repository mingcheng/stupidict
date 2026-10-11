// Package speech decodes offline MP3 audio and plays it through native devices.
package speech

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/hajimehoshi/go-mp3"
	dictionary "github.com/mingcheng/stupidict"
)

type Player struct {
	gate     sync.Mutex
	playGate sync.Mutex
	cancel   context.CancelFunc
	output   func(context.Context, []byte, int) error
}

func New() *Player {
	return &Player{output: playPCM}
}

func (player *Player) Stop() {
	player.gate.Lock()
	defer player.gate.Unlock()
	if player.cancel != nil {
		player.cancel()
		player.cancel = nil
	}
}

// Play blocks until completion, and belongs on a worker goroutine. A new call
// cancels the previous playback and waits for its device to be released.
func (player *Player) Play(ctx context.Context, audio []byte, word string, accent dictionary.Accent) error {
	if accent != dictionary.British && accent != dictionary.American {
		return errors.New("invalid accent")
	}
	player.gate.Lock()
	if err := ctx.Err(); err != nil {
		player.gate.Unlock()
		return err
	}
	if player.cancel != nil {
		player.cancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	player.cancel = cancel
	player.gate.Unlock()
	defer cancel()
	player.playGate.Lock()
	defer player.playGate.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(audio) == 0 {
		return fmt.Errorf("%w: %s (%s); install or update the audio pack", dictionary.ErrAudioNotFound, word, accent)
	}
	pcm, sampleRate, err := decodeMP3(ctx, audio)
	if err != nil {
		return err
	}
	return player.output(ctx, pcm, sampleRate)
}

func decodeMP3(ctx context.Context, audio []byte) ([]byte, int, error) {
	decoder, err := mp3.NewDecoder(contextReader{ctx, bytes.NewReader(audio)})
	if err != nil {
		return nil, 0, fmt.Errorf("decode MP3: %w", err)
	}
	const maxPCMBytes = 64 << 20
	pcm, err := io.ReadAll(io.LimitReader(contextReader{ctx, decoder}, maxPCMBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("decode MP3: %w", err)
	}
	if len(pcm) == 0 || len(pcm) > maxPCMBytes || len(pcm)%4 != 0 {
		return nil, 0, errors.New("invalid or oversized decoded MP3 audio")
	}
	return pcm, decoder.SampleRate(), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
