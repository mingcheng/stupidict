//go:build cgo

package speech

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gen2brain/malgo"
)

func TestPCMStreamDrainsAndPads(t *testing.T) {
	stream := pcmStream{data: []byte{1, 2, 3, 4}, drainBytes: 16, done: make(chan struct{})}
	output := bytes.Repeat([]byte{255}, 8)
	stream.fill(output)
	if !bytes.Equal(output, []byte{1, 2, 3, 4, 0, 0, 0, 0}) {
		t.Fatalf("PCM output: %v", output)
	}
	stream.fill(output)
	select {
	case <-stream.done:
		t.Fatal("finished before buffered audio drained")
	default:
	}
	stream.fill(output)
	select {
	case <-stream.done:
	default:
		t.Fatal("stream did not finish")
	}
	stream.fill(output)
	if !bytes.Equal(output, make([]byte, len(output))) {
		t.Fatal("finished stream did not produce silence")
	}
}

func TestNativePlaybackOnNullDevice(t *testing.T) {
	const miniaudioNullBackend = malgo.BackendNull + 1
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := playNative(ctx, make([]byte, 441*4), 44100, []malgo.Backend{miniaudioNullBackend}); err != nil {
		t.Fatal(err)
	}
	ctx, cancelPlayback := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelPlayback()
	if err := playNative(ctx, make([]byte, 44100*4), 44100, []malgo.Backend{miniaudioNullBackend}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("native cancellation: %v", err)
	}
}
