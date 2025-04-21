package libsamplerate

import (
	"encoding/binary"
	"fmt"
	"math"
)

// --- Constants ---
const (
	inputSampleRateUlaw = 8000.0
	outputSampleRatePCM = 16000.0
	channelsUlaw        = 1 // Assuming Mono input/output
	bytesPerOutputFrame = 2 // int16_t
)

// --- G.711 u-Law Decoder (Matches C++ version) ---
var ulawExpLut = [8]int16{0, 132, 396, 924, 1980, 4092, 8316, 16764}

// ulawToLinearInt16Go decodes a single u-law byte to its 16-bit linear PCM equivalent.
func ulawToLinearInt16Go(ulawByte byte) int16 {
	ulaw := ^ulawByte // Invert bits

	sign := (ulaw & 0x80)
	exponent := (ulaw >> 4) & 0x07
	mantissa := ulaw & 0x0F

	// Calculate magnitude from exponent lookup and mantissa shift
	linearVal := ulawExpLut[exponent] + (int16(mantissa) << (exponent + 3))

	// Apply sign (match C++ where sign=0 is negative after inversion)
	if sign == 0 {
		linearVal = -linearVal
	}

	// The table/formula should keep it within 14-bit range, fitting in int16.
	// C++ version had commented-out clamping, likely not needed.
	return linearVal
}

// ConvertUlawToPCM converts a slice of u-law encoded bytes (at 8kHz) to
// a slice of 16-bit little-endian PCM bytes resampled to 16kHz.
//
// Args:
//
//	inputUlaw: Slice of bytes containing u-law encoded audio data (8kHz).
//	quality: The libsamplerate converter quality type (e.g., SincBestQuality).
//
// Returns:
//
//	A slice of bytes containing 16-bit little-endian PCM audio data (16kHz),
//	or nil and an error if conversion fails.
func ConvertUlawToPCM(inputUlaw []byte, quality ConverterType) ([]byte, error) {

	if len(inputUlaw) == 0 {
		return []byte{}, nil // Return empty slice for empty input
	}

	// --- libsamplerate Setup ---
	const srcRatio = outputSampleRatePCM / inputSampleRateUlaw // Should be 2.0
	var state Converter                                        // Use interface
	var err error                                              // Declare err variable

	state, err = New(quality, channelsUlaw)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize libsamplerate: %w", err)
	}
	defer state.Close() // Ensure cleanup

	// --- Prepare Input Data (u-Law -> float32) ---
	totalInputFrames := len(inputUlaw)
	inputFloatBuffer := make([]float32, totalInputFrames*channelsUlaw) // Size for mono
	const scaleToFloat = 1.0 / 32768.0                                 // Consistent scaling factor

	for i := 0; i < totalInputFrames; i++ {
		sampleS16 := ulawToLinearInt16Go(inputUlaw[i])
		inputFloatBuffer[i] = float32(sampleS16) * scaleToFloat
	}

	// --- Prepare Output Buffers ---
	estimatedMaxOutputFrames := int64(math.Ceil(float64(totalInputFrames)*srcRatio)) + 20 // Add headroom
	outputFloatBuffer := make([]float32, estimatedMaxOutputFrames*int64(channelsUlaw))
	// Final byte slice - pre-allocate capacity
	outputPcmBytes := make([]byte, 0, estimatedMaxOutputFrames*int64(channelsUlaw)*int64(bytesPerOutputFrame))
	// Temporary buffer for byte conversion in the loop
	byteBuf := make([]byte, bytesPerOutputFrame)

	// --- Perform Resampling (Single Pass + Flush) ---
	srcData := SrcData{
		DataIn:       inputFloatBuffer,
		InputFrames:  int64(totalInputFrames),
		DataOut:      outputFloatBuffer,
		OutputFrames: estimatedMaxOutputFrames,
		SrcRatio:     srcRatio,
		EndOfInput:   true, // Indicate all input is provided
	}

	// Process the main block of data
	err = state.Process(&srcData)
	if err != nil {
		return nil, fmt.Errorf("libsamplerate src_process failed: %w", err)
	}

	framesGenerated := srcData.OutputFramesGen

	// Convert generated float samples to S16 bytes and append
	if framesGenerated > 0 {
		outputPcmBytes = appendFloatToBytesPCM16LE(outputPcmBytes, outputFloatBuffer[:framesGenerated*int64(channelsUlaw)], byteBuf)
	}

	// --- Flush any remaining samples from libsamplerate ---
	srcData.DataIn = nil // No more input data
	srcData.InputFrames = 0

	for {
		// Reset output frames generated for this Process call
		srcData.OutputFramesGen = 0
		// Set output buffer and capacity for this flush call
		srcData.DataOut = outputFloatBuffer             // Reuse buffer
		srcData.OutputFrames = estimatedMaxOutputFrames // Max it can hold

		err = state.Process(&srcData)
		if err != nil {
			return nil, fmt.Errorf("libsamplerate src_process (flush) failed: %w", err)
		}

		framesGenerated = srcData.OutputFramesGen

		if framesGenerated <= 0 {
			break // No more frames generated, flush complete
		}

		// Convert and append flushed frames
		outputPcmBytes = appendFloatToBytesPCM16LE(outputPcmBytes, outputFloatBuffer[:framesGenerated*int64(channelsUlaw)], byteBuf)

	} // End flush loop

	return outputPcmBytes, nil
}

// appendFloatToBytesPCM16LE converts a slice of float32 to int16, then appends
// the resulting bytes (Little Endian) to an existing byte slice.
// Uses scaling by 32767 and clamping, matching the C++ code.
func appendFloatToBytesPCM16LE(dest []byte, src []float32, byteBuf []byte) []byte {
	if len(byteBuf) < bytesPerOutputFrame {
		// Allocate if not provided or too small
		byteBuf = make([]byte, bytesPerOutputFrame)
	}

	for _, sampleF := range src {
		// Clamp float32 sample to [-1.0, 1.0]
		if sampleF > 1.0 {
			sampleF = 1.0
		}
		if sampleF < -1.0 {
			sampleF = -1.0
		}

		// Scale to int16 range using 32767 (matching C++) and cast
		sampleS16 := int16(sampleF * 32767.0)

		// Convert int16 to little-endian bytes
		binary.LittleEndian.PutUint16(byteBuf, uint16(sampleS16))

		// Append the 2 bytes to the destination slice
		dest = append(dest, byteBuf...)
	}
	return dest
}

// Note: You might need to add/ensure these exist in your package:
// type ConverterType int
// const ( SincBestQuality ConverterType = ... )
// type Converter interface { ... }
// type SrcData struct { ... }
// func New(...) (Converter, error)
// func Simple(...) error             // Not used here, but likely present
// func FloatToShortArray(...)        // Not used here, replaced by custom logic
