//
// Copyright (c) 2025, Antonio Chirizzi <antonio.chirizzi@gmail.com>
// All rights reserved.
//
// This code is released under 3-clause BSD license. Please see the
// file LICENSE
//

package libsamplerate

import (
	"encoding/binary"
	"fmt"
	"math"
)

// --- Constants ---
const (
	mixInputSampleRate     = 24000.0
	mixOutputSampleRate    = 8000.0
	mixChannels            = 1   // Assuming Mono input/output
	mixBytesPerInputFrame  = 2   // S16LE
	mixBytesPerOutputFrame = 1   // u-Law
	mixFactorDefault       = 0.6 // Default mix factor
)

// --- Helper: S16LE Bytes to int16 ---
func bytesToS16LEGo(buffer []byte, byteIndex int) (int16, error) {
	// Bounds check
	if byteIndex < 0 || byteIndex+1 >= len(buffer) {
		// Return error instead of printing warning like C++ helper
		return 0, fmt.Errorf("bytesToS16LEGo read out of bounds: index %d for buffer size %d", byteIndex, len(buffer))
	}
	// Read 2 bytes using LittleEndian encoding
	return int16(binary.LittleEndian.Uint16(buffer[byteIndex:])), nil
}

// --- Helper: int16 to float32 [-1.0, 1.0) ---
func s16ToFloatGo(sampleS16 int16) float32 {
	// Use 32768.0 for normalization to avoid reaching exactly 1.0
	return float32(sampleS16) / 32768.0
}

// --- Helper: int16 to u-Law byte (G.711) ---
// Matches the C++ linear_to_ulaw function
func linearToUlawGo(pcmVal int16) byte {
	const (
		pcmMax = 32767
		bias   = 0x84 // 132
		clip   = 32635
	)
	var uVal byte
	var sign int
	var pcmMag int // Use int for intermediate magnitude calculations

	if pcmVal < 0 {
		sign = 0
		pcmMag = int(-pcmVal)
	} else {
		sign = 0x80
		pcmMag = int(pcmVal)
	}

	if pcmMag > clip {
		pcmMag = clip // Clip magnitude
	}
	// Add bias before finding segment/mantissa
	pcmMag += bias

	// Find exponent segment (0-7)
	exponent := 7
	for expMask := 0x4000; (pcmMag&expMask) == 0 && exponent > 0; exponent-- {
		expMask >>= 1
	}

	// Extract mantissa
	mantissa := (pcmMag >> (exponent + 3)) & 0x0F // Shift right based on exponent, take lower 4 bits

	// Combine sign, exponent, mantissa
	uVal = byte(sign | (exponent << 4) | mantissa)

	// Invert bits for standard u-Law
	return ^uVal
}

// MixResampleUlaw mixes two S16LE 24kHz PCM audio streams, resamples to 8kHz,
// and converts to u-Law. It updates lastSample2MixedPos with the index of the
// last sample used from pcmStream2 + 1 (wrapping if necessary).
//
// Args:
//
//	pcmStream1: Byte slice containing the first S16LE 24kHz PCM stream.
//	pcmStream2: Byte slice containing the second S16LE 24kHz PCM stream.
//	lastSample2MixedPos: Pointer to an int storing the starting index (0-based) for reading pcmStream2.
//	                     This value is updated on successful completion.
//	mixFactor: The scaling factor applied to each stream before adding (0.0 to 1.0).
//
// Returns:
//
//	A byte slice containing the resulting 8kHz u-Law audio data, or nil and an error.
func MixResampleUlaw(
	pcmStream1, pcmStream2 []byte,
	lastSample2MixedPos *int, // Pointer to track position
	mixFactor float32,
) ([]byte, error) {

	// --- Input Validation ---
	if len(pcmStream1)%mixBytesPerInputFrame != 0 {
		return nil, fmt.Errorf("input stream 1 size (%d) not multiple of frame size (%d)", len(pcmStream1), mixBytesPerInputFrame)
	}
	if len(pcmStream2)%mixBytesPerInputFrame != 0 {
		return nil, fmt.Errorf("input stream 2 size (%d) not multiple of frame size (%d)", len(pcmStream2), mixBytesPerInputFrame)
	}
	if lastSample2MixedPos == nil {
		return nil, fmt.Errorf("lastSample2MixedPos pointer must not be nil")
	}

	frames1 := len(pcmStream1) / mixBytesPerInputFrame
	frames2 := len(pcmStream2) / mixBytesPerInputFrame
	totalInputFrames := frames1 // Process for the duration of stream 1

	if totalInputFrames == 0 {
		fmt.Println("MixResampleUlaw: Warning: Input stream 1 is empty. Returning empty output.")
		// Do not update lastSample2MixedPos if no processing happens
		return []byte{}, nil
	}
	if frames2 == 0 {
		fmt.Println("MixResampleUlaw: Warning: Input stream 2 is empty. Mixing only stream 1.")
		// Allow proceeding, but stream 2 samples will be 0.0
	}

	// Validate and adjust starting position for stream 2
	startPos2 := *lastSample2MixedPos + 1
	if frames2 > 0 { // Only wrap if stream 2 has frames
		if startPos2 < 0 || startPos2 >= frames2 {
			fmt.Printf("MixResampleUlaw: Info: Wrapping stream 2 index (%d -> 0), frames2=%d\n", startPos2, frames2)
			startPos2 = 0
		}
	} else {
		startPos2 = 0 // If stream 2 is empty, always start at 0 conceptually
	}
	// fmt.Printf("MixResampleUlaw: DEBUG: Mixing %d frames. Stream 2 starts at index %d (frames2=%d).\n", totalInputFrames, startPos2, frames2)

	// --- libsamplerate Setup ---
	const srcRatio = mixOutputSampleRate / mixInputSampleRate // 1.0 / 3.0
	var state Converter
	var err error

	// C++ code hardcoded best quality, let's match that
	state, err = New(SincBestQuality, mixChannels)
	if err != nil {
		return nil, fmt.Errorf("ERROR: src_new() failed: %w", err)
	}
	defer state.Close()

	// --- Buffers ---
	mixedFloatBuffer := make([]float32, totalInputFrames*mixChannels)
	estimatedOutputFrames := int64(math.Ceil(float64(totalInputFrames)*srcRatio)) + 20
	outputFloatBuffer := make([]float32, estimatedOutputFrames*int64(mixChannels))
	resultUlawVector := make([]byte, 0, estimatedOutputFrames*int64(mixChannels)) // Capacity only

	// --- Mixing ---
	i2 := startPos2 // Current index for stream 2
	for i1 := 0; i1 < totalInputFrames; i1++ {
		byteIndex1 := i1 * mixBytesPerInputFrame
		byteIndex2 := i2 * mixBytesPerInputFrame

		var s16_1, s16_2 int16
		var err1, err2 error
		var sample1F, sample2F float32

		// Stream 1 sample (always exists within loop bounds)
		s16_1, err1 = bytesToS16LEGo(pcmStream1, byteIndex1)
		if err1 != nil {
			return nil, fmt.Errorf("error reading stream 1 at index %d: %w", byteIndex1, err1)
		} // Should not happen
		sample1F = s16ToFloatGo(s16_1)

		// Stream 2 sample (only if stream 2 has frames and index is valid)
		if frames2 > 0 {
			s16_2, err2 = bytesToS16LEGo(pcmStream2, byteIndex2)
			if err2 != nil {
				return nil, fmt.Errorf("error reading stream 2 at index %d: %w", byteIndex2, err2)
			} // Should not happen
			sample2F = s16ToFloatGo(s16_2)
		} // else sample2F remains 0.0

		// Mix and store (already scaled)
		mixedFloatBuffer[i1] = sample1F*mixFactor + sample2F*mixFactor

		// Advance and wrap stream 2 index
		if frames2 > 0 { // Only advance if stream 2 has content
			i2++
			if i2 >= frames2 {
				i2 = 0 // Wrap around
			}
		}
	}
	// Update the position pointer with the *next* index to be used from stream 2
	*lastSample2MixedPos = i2
	// fmt.Printf("MixResampleUlaw: DEBUG: Mixing complete. Next stream 2 index: %d\n", *lastSample2MixedPos)

	// --- Resampling ---
	srcData := SrcData{
		DataIn:       mixedFloatBuffer,
		InputFrames:  int64(totalInputFrames),
		DataOut:      outputFloatBuffer,
		OutputFrames: int64(len(outputFloatBuffer)), // Capacity
		SrcRatio:     srcRatio,
		EndOfInput:   true, // Process all input at once
	}

	err = state.Process(&srcData)
	if err != nil {
		return nil, fmt.Errorf("ERROR: src_process() failed during main processing: %w", err)
	}

	framesGenerated := srcData.OutputFramesGen
	// fmt.Printf("MixResampleUlaw: DEBUG: Resampling generated %d frames.\n", framesGenerated)

	// --- Convert and Store Output (First Pass) ---
	if framesGenerated > 0 {
		resultUlawVector = appendPCMFloatToUlawBytes(resultUlawVector, outputFloatBuffer[:framesGenerated*int64(mixChannels)])
	}

	// --- Flush Resampler ---
	// fmt.Printf("MixResampleUlaw: DEBUG: Flushing resampler...\n")
	srcData.DataIn = nil // No more input
	srcData.InputFrames = 0
	totalFlushedFrames := int64(0)

	for {
		srcData.DataOut = outputFloatBuffer                  // Reuse buffer
		srcData.OutputFrames = int64(len(outputFloatBuffer)) // Capacity
		srcData.OutputFramesGen = 0                          // Reset before call

		err = state.Process(&srcData)
		if err != nil {
			return nil, fmt.Errorf("ERROR: src_process() failed during flush: %w", err)
		}

		framesGenerated = srcData.OutputFramesGen
		totalFlushedFrames += framesGenerated

		if framesGenerated <= 0 {
			break // No more output from flush
		}

		resultUlawVector = appendPCMFloatToUlawBytes(resultUlawVector, outputFloatBuffer[:framesGenerated*int64(mixChannels)])

	}
	// fmt.Printf("MixResampleUlaw: DEBUG: Flushing generated additional %d frames.\n", totalFlushedFrames)

	// fmt.Printf("MixResampleUlaw: DEBUG: Processing complete. Total output u-Law bytes: %d\n", len(resultUlawVector))
	return resultUlawVector, nil
}

// appendPCMFloatToUlawBytes converts float32 samples (already resampled)
// to u-Law bytes and appends them to the destination slice.
// Uses clamping and scaling by 32767 before u-Law encoding.
func appendPCMFloatToUlawBytes(dest []byte, src []float32) []byte {
	// Ensure capacity if possible (though append handles reallocation)
	if cap(dest)-len(dest) < len(src) {
		// Grow destination capacity - conservative estimate
		newDest := make([]byte, len(dest), len(dest)+len(src))
		copy(newDest, dest)
		dest = newDest
	}

	for _, sampleF := range src {
		// Clamp float32 sample to [-1.0, 1.0]
		if sampleF > 1.0 {
			sampleF = 1.0
		}
		if sampleF < -1.0 {
			sampleF = -1.0
		}

		// Scale to int16 range using 32767 and cast
		sampleS16 := int16(sampleF * 32767.0)

		// Convert int16 to u-Law byte
		ulawByte := linearToUlawGo(sampleS16)

		// Append the u-Law byte
		dest = append(dest, ulawByte)
	}
	return dest
}

// Optional wrapper with default mix factor
func MixResampleUlawDefaultFactor(
	pcmStream1, pcmStream2 []byte,
	lastSample2MixedPos *int,
) ([]byte, error) {
	return MixResampleUlaw(pcmStream1, pcmStream2, lastSample2MixedPos, mixFactorDefault)
}
