//
// Copyright (c) 2025, Antonio Chirizzi <antonio.chirizzi@gmail.com>
// All rights reserved.
//
// This code is released under 3-clause BSD license. Please see the
// file LICENSE
//

package libsamplerate

import (
	"fmt"
	"math"
	// Add other necessary imports for helpers, e.g., "testing" if helpers use t.Helper()
	// Add "gonum.org/v1/gonum/dsp/fourier", "math/cmplx", "sort" if moving calculateSnrGo here
)

// --- Shared Test Helper Functions ---

const (
	maxChannelsMulti = 10 // From C MAX_CHANNELS
)

// genWindowedSinesGo generates windowed sine waves. Direct translation of C version.
func genWindowedSinesGo(freqCount int, freqs []float64, maxAmp float64, output []float32) {
	outputLen := len(output)
	if outputLen <= 1 || freqCount <= 0 {
		for i := range output {
			output[i] = 0.0
		}
		return
	}

	for i := range output {
		output[i] = 0.0
	} // Zero slice

	amplitude := maxAmp / float64(freqCount)
	outputLenF := float64(outputLen)

	for freqIdx := 0; freqIdx < freqCount; freqIdx++ {
		freqVal := freqs[freqIdx]
		if freqVal <= 0.0 || freqVal >= 0.5 {
			panic(fmt.Sprintf("genWindowedSinesGo: Error: freq [%d] == %g is out of range (0.0, 0.5).", freqIdx, freqVal))
		}
		phase := 0.9 * math.Pi / float64(freqCount) // Constant phase from C

		for k := 0; k < outputLen; k++ {
			kF := float64(k)
			output[k] += float32(amplitude * math.Sin(freqVal*(2.0*kF)*math.Pi+phase))
		}
	}

	// Apply Hanning Window
	denominator := outputLenF - 1.0
	for k := 0; k < outputLen; k++ {
		kF := float64(k)
		window := 0.5 - 0.5*math.Cos((2.0*kF)*math.Pi/denominator)
		output[k] *= float32(window)
	}
}

// findPeakGo corresponds to find_peak() in C
func findPeakGo(data []float32) float64 {
	peak := 0.0
	for _, val := range data {
		absVal := math.Abs(float64(val))
		if absVal > peak {
			peak = absVal
		}
	}
	return peak
}

// absInt64 calculates the absolute value of an int64.
func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// minInt64 returns the smaller of two int64 values.
func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// interleaveDataGo converts separate channel buffers into a single interleaved buffer.
// 'in' is a slice where each element is a slice representing one channel's data.
// 'out' is the destination interleaved buffer.
// Assumes len(in) == channels and len(in[0]) == frames (and all channels have same length).
// Assumes len(out) >= frames * channels.
func interleaveDataGo(in [][]float32, out []float32, frames int, channels int) error {
	if len(in) != channels {
		return fmt.Errorf("interleaveDataGo: input slice count (%d) != channels (%d)", len(in), channels)
	}
	if channels == 0 || frames == 0 {
		return nil
	} // Nothing to do
	if len(in[0]) < frames {
		return fmt.Errorf("interleaveDataGo: input channel 0 length (%d) < frames (%d)", len(in[0]), frames)
	}
	if len(out) < frames*channels {
		return fmt.Errorf("interleaveDataGo: output buffer too small (%d < %d)", len(out), frames*channels)
	}

	outIdx := 0
	for fr := 0; fr < frames; fr++ {
		for ch := 0; ch < channels; ch++ {
			if fr >= len(in[ch]) { // Check bounds for each channel frame
				return fmt.Errorf("interleaveDataGo: input channel %d length (%d) < frames (%d) at frame %d", ch, len(in[ch]), frames, fr)
			}
			out[outIdx] = in[ch][fr]
			outIdx++
		}
	}
	return nil
}

// deinterleaveDataGo converts an interleaved buffer into separate channel buffers.
// 'in' is the source interleaved buffer (length >= frames * channels).
// 'out' is the destination slice-of-slices (must be pre-allocated with correct dimensions).
// Assumes len(out) == channels and len(out[0]) >= frames.
func deinterleaveDataGo(in []float32, out [][]float32, frames int, channels int) error {
	if len(out) != channels {
		return fmt.Errorf("deinterleaveDataGo: output slice count (%d) != channels (%d)", len(out), channels)
	}
	if channels == 0 || frames == 0 {
		return nil
	} // Nothing to do
	if len(in) < frames*channels {
		return fmt.Errorf("deinterleaveDataGo: input buffer too small (%d < %d)", len(in), frames*channels)
	}
	if len(out[0]) < frames {
		return fmt.Errorf("deinterleaveDataGo: output channel 0 length (%d) < frames (%d)", len(out[0]), frames)
	}

	inIdx := 0
	for fr := 0; fr < frames; fr++ {
		for ch := 0; ch < channels; ch++ {
			if fr >= len(out[ch]) { // Check bounds for each channel frame
				return fmt.Errorf("deinterleaveDataGo: output channel %d length (%d) < frames (%d) at frame %d", ch, len(out[ch]), frames, fr)
			}
			out[ch][fr] = in[inIdx]
			inIdx++
		}
	}
	return nil
}
