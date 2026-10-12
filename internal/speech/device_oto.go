/*!*
 * Copyright (c) 2026 Ming Lyu, aka mingcheng
 *
 * This source code is licensed under the MIT License,
 * which is located in the LICENSE file in the source tree's root directory.
 *
 * File: device_oto.go
 * Author: mingcheng <mingcheng@apache.org>
 * File Created: 2026-10-12 19:48:07
 *
 * Modified By: mingcheng <mingcheng@apache.org>
 * Last Modified: 2026-10-12 19:48:10
 */

package speech

import (
	"io"

	"github.com/ebitengine/oto/v3"
)

// otoDevice adapts Oto, which drives CoreAudio, WASAPI/WinMM, and
// PulseAudio/ALSA without CGO.
type otoDevice struct {
	context    *oto.Context
	ready      chan struct{}
	sampleRate int
}

func openOto(sampleRate int) (device, error) {
	context, ready, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:      sampleRate,
		ChannelCount:    2,
		Format:          oto.FormatSignedInt16LE,
		ApplicationName: "stupidict",
	})
	if err != nil {
		return nil, err
	}
	return &otoDevice{context: context, ready: ready, sampleRate: sampleRate}, nil
}

func (device *otoDevice) SampleRate() int            { return device.sampleRate }
func (device *otoDevice) Ready() <-chan struct{}     { return device.ready }
func (device *otoDevice) NewVoice(r io.Reader) voice { return device.context.NewPlayer(r) }
func (device *otoDevice) Suspend() error             { return device.context.Suspend() }
func (device *otoDevice) Resume() error              { return device.context.Resume() }
func (device *otoDevice) Err() error                 { return device.context.Err() }
