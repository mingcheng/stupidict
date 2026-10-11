//go:build !cgo

package speech

import (
	"context"
	"errors"
)

func playPCM(ctx context.Context, _ []byte, _ int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("native audio requires CGO; rebuild with make build CGO_ENABLED=1")
}
