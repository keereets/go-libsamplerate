//go:build fftw_required

// Needed because varispeed_test calls calculateSnrGo
// go test -tags fftw_required ./... -v

package libsamplerate

import (
	"fmt"
	"math"
	"testing"
	// Assumes types, constants, New, Process, Simple, etc. are defined
	// Assumes helpers genWindowedSinesGo, calculateSnrGo, reverseDataGo are in test_utils.go
)

const (
	bufferLenVarispeed = 1 << 14 // 16384
)

// TestVarispeedAPI corresponds to main() in varispeed_test.c
func TestVarispeedAPI(t *testing.T) {
	// Note: Varispeed tests use src_process and src_set_ratio, exercising different paths

	// Varispeed SNR Test
	t.Run("VarispeedSNR", func(t *testing.T) {
		tests := []struct {
			name      string
			converter ConverterType
			enabled   bool
			targetSnr float64
		}{
			{"ZeroOrderHold", ZeroOrderHold, true, 10.0},
			{"Linear", Linear, true, 10.0},
			{"SincFastest", SincFastest, enableSincFastConverter, 115.0}, // C used 115.0? Might need adjustment like snr_test
			// Add Medium/Best if enabled and desired
		}
		for _, tt := range tests {
			tc := tt // Capture range variable
			if !tc.enabled {
				t.Logf("Skipping Varispeed SNR test for %s (disabled)", tc.name)
				continue
			}
			t.Run(tc.name, func(t *testing.T) {
				// t.Parallel() // Possible if converter instances are independent
				testVarispeed(t, tc.converter, tc.targetSnr)
			})
		}
	})

	// Varispeed Bounds Test (Set Ratio Test)
	t.Run("VarispeedBounds", func(t *testing.T) {
		tests := []struct {
			name      string
			converter ConverterType
			enabled   bool
		}{
			{"ZeroOrderHold", ZeroOrderHold, true},
			{"Linear", Linear, true},
			{"SincFastest", SincFastest, enableSincFastConverter},
			// Add Medium/Best if enabled and desired
		}
		for _, tt := range tests {
			tc := tt // Capture range variable
			if !tc.enabled {
				t.Logf("Skipping Varispeed Bounds test for %s (disabled)", tc.name)
				continue
			}
			t.Run(tc.name, func(t *testing.T) {
				// t.Parallel() // Possible if converter instances are independent
				testVarispeedBounds(t, tc.converter)
			})
		}
	})

	// No fftw_cleanup needed
}

// testVarispeed corresponds to varispeed_test() in C
func testVarispeed(t *testing.T, converter ConverterType, targetSnr float64) {
	t.Helper()
	logPrefix := fmt.Sprintf("Varispeed SNR (%s): ", GetName(converter))
	t.Logf("%s Starting...", logPrefix)

	// Allocate buffers slightly larger for safety margin with Process
	bufCap := bufferLenVarispeed + 100
	input := make([]float32, bufCap) // Use full cap for first pass output
	output := make([]float32, bufCap)

	inputLenFirstPass := bufferLenVarispeed / 2 // Matches C: ARRAY_LEN / 2

	// Generate initial signal
	sineFreq := 0.0111
	genWindowedSinesGo(1, []float64{sineFreq}, 1.0, input[:inputLenFirstPass]) // Fill only first half

	// --- First Pass: Upsample ratio 3.0 ---
	const ratio1 = 3.0
	// Set ratio for downsampling *next* (mistake in C comment/logic?) C sets ratio for the *current* operation.
	// src_set_ratio(1.0/3.0) means ratio *IS* 1/3? Let's re-read src_process context. NO, src_process uses src_data.src_ratio.
	// src_set_ratio updates internal state for *future* calls if ratio changes. The C code is confusing here. It sets src_data.src_ratio=3.0,
	// then calls src_set_ratio(1.0/3.0), then calls src_process.
	// Does src_process use src_data.src_ratio or state->last_ratio? Looking at samplerate.go->Process, it uses state->last_ratio
	// if it's valid, otherwise data->SrcRatio, and compares them to choose const/vari path. This implies src_set_ratio *does*
	// affect the current call if it's the first call after New/Reset. Let's assume C intended to run first pass at ratio 1.0/3.0.
	// const setRatio1 = 1.0 / ratio1
	const actualRatio1 = 1.0 / 3.0

	state, err := New(converter, 1) // 1 Channel
	if err != nil {
		t.Fatalf("%s New() failed: %v (C Line ~97)", logPrefix, err)
	}
	defer state.Close() // Ensure cleanup

	// Prepare SrcData for first pass
	srcData1 := SrcData{
		DataIn:       input[:inputLenFirstPass], // Slice containing the sine wave
		InputFrames:  int64(inputLenFirstPass),
		DataOut:      output, // Full output buffer capacity
		OutputFrames: int64(len(output)),
		SrcRatio:     actualRatio1, // C had 3.0 here, but called set_ratio(1/3.0). Assume 1/3.0 is intended effective ratio.
		EndOfInput:   true,         // Process all input at once
	}

	// Call src_set_ratio - although potentially redundant if Process uses data.SrcRatio on first call
	// Let's include it to match C structure.
	err = state.SetRatio(actualRatio1) // Set internal ratio
	if err != nil {
		t.Fatalf("%s SetRatio(%.4f) failed: %v (C Line ~113)", logPrefix, actualRatio1, err)
	}

	// Perform first conversion
	err = state.Process(&srcData1)
	if err != nil {
		t.Fatalf("%s Process() pass 1 failed: %v (C Line ~119)", logPrefix, err)
	}

	// Check input consumption for first pass
	if srcData1.InputFramesUsed != int64(inputLenFirstPass) {
		t.Errorf("%s Pass 1 did not consume all input: Used %d, Expected %d (C Line ~126)", logPrefix, srcData1.InputFramesUsed, inputLenFirstPass)
	}
	outputLenPass1 := int(srcData1.OutputFramesGen)
	if outputLenPass1 <= 0 {
		t.Fatalf("%s Pass 1 generated no output", logPrefix)
	}
	t.Logf("%s Pass 1 generated %d frames", logPrefix, outputLenPass1)

	// --- Prepare for Second Pass ---
	// Copy output back to input buffer
	if outputLenPass1 > len(input) {
		t.Fatalf("%s Pass 1 output (%d) exceeds input buffer capacity (%d)", logPrefix, outputLenPass1, len(input))
	}
	copy(input[:outputLenPass1], output[:outputLenPass1]) // Copy valid output

	// Reverse the data
	reverseDataGo(input[:outputLenPass1]) // Use helper from test_utils.go

	// Reset the converter state
	err = state.Reset()
	if err != nil {
		t.Fatalf("%s Reset() failed: %v (C Line ~138)", logPrefix, err)
	}

	// --- Second Pass: Reverse ratio (3.0) ---
	const actualRatio2 = ratio1 // Should be 3.0

	inputLenSecondPass := outputLenPass1 // Input is the output of the first pass

	// Prepare SrcData for second pass
	srcData2 := SrcData{
		DataIn:       input[:inputLenSecondPass], // Reversed data
		InputFrames:  int64(inputLenSecondPass),
		DataOut:      output,             // Reuse output buffer
		OutputFrames: int64(len(output)), // Full capacity
		SrcRatio:     actualRatio2,
		EndOfInput:   true,
	}

	// Set ratio for second pass
	err = state.SetRatio(actualRatio2)
	if err != nil {
		t.Fatalf("%s SetRatio(%.4f) failed: %v (C Line ~152)", logPrefix, actualRatio2, err)
	}

	// Perform second conversion
	err = state.Process(&srcData2)
	if err != nil {
		t.Fatalf("%s Process() pass 2 failed: %v (C Line ~158)", logPrefix, err)
	}

	// Check input consumption for second pass
	if srcData2.InputFramesUsed != int64(inputLenSecondPass) {
		t.Errorf("%s Pass 2 did not consume all input: Used %d, Expected %d (C Line ~165)", logPrefix, srcData2.InputFramesUsed, inputLenSecondPass)
	}
	outputLenPass2 := int(srcData2.OutputFramesGen)
	if outputLenPass2 <= 0 {
		t.Fatalf("%s Pass 2 generated no output", logPrefix)
	}
	t.Logf("%s Pass 2 generated %d frames", logPrefix, outputLenPass2)

	// --- Calculate SNR of final output ---
	// State is closed automatically by defer
	finalOutput := output[:outputLenPass2]               // Slice to actual output
	snr, snrErr := calculateSnrGo(finalOutput, 1, false) // Use function from snr_bw_test / test_utils

	if snrErr != nil {
		t.Errorf("%s calculateSnrGo failed: %v (C Line ~174)", logPrefix, snrErr)
	} else {
		t.Logf("%s Calculated SNR = %.2f dB (Target >= %.1f)", logPrefix, snr, targetSnr)
		if snr < targetSnr {
			t.Errorf("%s SNR too low: Got %.2f dB, expected >= %.1f dB (C Line ~177)", logPrefix, snr, targetSnr)
			// saveOctFloatGo("varispeed.mat", input[:inputLenSecondPass], finalOutput) // Optional save
		}
	}

	if !t.Failed() {
		t.Logf("%s ok", logPrefix)
	}
}

// testVarispeedBounds corresponds to varispeed_bounds_test() in C
func testVarispeedBounds(t *testing.T, converter ConverterType) {
	t.Helper()
	logPrefix := fmt.Sprintf("Varispeed Bounds (%s): ", GetName(converter))
	t.Logf("%s Starting...", logPrefix)

	ratios := []float64{0.1, 0.01, 20.0} // Ratios from C test (added .0 for clarity)

	// Loop channels 1 to 9
	for chanCount := 1; chanCount <= 9; chanCount++ {
		currentChanCount := chanCount // Capture
		t.Run(fmt.Sprintf("Channels_%d", currentChanCount), func(t *testing.T) {
			// Nested loops for ratio pairs
			for r1 := 0; r1 < len(ratios); r1++ {
				for r2 := 0; r2 < len(ratios); r2++ {
					if r1 == r2 {
						continue
					} // Skip if ratios are the same

					ratio1 := ratios[r1] // Capture
					ratio2 := ratios[r2] // Capture
					t.Run(fmt.Sprintf("Ratio_%.2f_to_%.2f", ratio1, ratio2), func(t *testing.T) {
						// t.Parallel() // Maybe possible? Needs careful check of testSetRatio state handling.
						testSetRatio(t, converter, currentChanCount, ratio1, ratio2)
					})
				}
			}
		})
	}
	t.Logf("%s ok", logPrefix) // C prints ok after the outer loops
}

// testSetRatio corresponds to set_ratio_test() in C
func testSetRatio(t *testing.T, converter ConverterType, channels int, initialRatio, secondRatio float64) {
	t.Helper()
	logPrefix := fmt.Sprintf("SetRatio Test (Ch=%d, %.2f->%.2f): ", channels, initialRatio, secondRatio)
	// t.Logf("%s Starting...", logPrefix) // Can be noisy

	const totalInputFrames = bufferLenVarispeed // Use smaller buffer from varispeed_test? C uses BUFFER_LEN here. Let's stick to C.
	// Estimate max output based on largest possible ratio (20x) + margin
	maxRatioLocal := math.Max(initialRatio, secondRatio)
	maxRatioOverall := math.Max(maxRatioLocal, 25.0)                                        // Based on C comment/constant
	totalOutputFramesCap := int(math.Ceil(float64(totalInputFrames)*maxRatioOverall)) + 200 // Generous capacity
	const chunkSize = 128

	// Use make instead of calloc/free
	input := make([]float32, totalInputFrames*channels) // Input is all zeros
	output := make([]float32, totalOutputFramesCap*channels)

	// --- Init State ---
	state, err := New(converter, channels)
	if err != nil {
		t.Fatalf("%s New() failed: %v (C Line ~217)", logPrefix, err)
	}
	defer state.Close()

	var totalFramesUsed int64 = 0
	var totalFramesGen int64 = 0
	currentRatio := initialRatio // Start with initial ratio

	srcData := SrcData{
		SrcRatio: currentRatio,
		// Other fields set in loop
	}

	// Safety break for loops
	maxLoopCount := (totalInputFrames / chunkSize) + int(math.Ceil(float64(totalOutputFramesCap)/float64(chunkSize))) + 100

	for k := 0; k < maxLoopCount; k++ {

		// Change ratio after first chunk (k=1)
		if k == 1 {
			currentRatio = secondRatio
			err = state.SetRatio(secondRatio)
			if err != nil {
				t.Fatalf("%s SetRatio(%.2f) failed: %v (C Line ~240)", logPrefix, secondRatio, err)
			}
			srcData.SrcRatio = secondRatio // Also update SrcData for Process call's initial check
		}

		// Calculate remaining input for this chunk
		remainingInput := int64(totalInputFrames) - totalFramesUsed
		inFramesThisBlock := minInt64(int64(chunkSize), remainingInput)
		if inFramesThisBlock < 0 {
			inFramesThisBlock = 0
		}

		// Calculate output capacity for this chunk (provide remaining capacity)
		outSpaceAvailable := int64(len(output)/channels) - totalFramesGen // Remaining frame capacity
		outFramesThisBlock := outSpaceAvailable
		if outFramesThisBlock < 0 {
			outFramesThisBlock = 0
		}

		// Set End Of Input flag
		srcData.EndOfInput = (totalFramesUsed >= int64(totalInputFrames))
		if srcData.EndOfInput {
			inFramesThisBlock = 0
		} // Ensure 0 input frames if EOF

		srcData.InputFrames = inFramesThisBlock
		srcData.OutputFrames = outFramesThisBlock

		// Prepare slices
		inStart := int(totalFramesUsed) * channels
		inEnd := inStart + int(inFramesThisBlock)*channels
		if inStart > len(input) {
			inStart = len(input)
		}
		if inEnd > len(input) {
			inEnd = len(input)
		}
		if inStart > inEnd {
			inStart = inEnd
		}

		outStart := int(totalFramesGen) * channels
		outEnd := outStart + int(outFramesThisBlock)*channels // Use full available capacity? C uses total_output_frames - total_frames_gen
		if outStart > len(output) {
			outStart = len(output)
		}
		if outEnd > len(output) {
			outEnd = len(output)
		} // Cap slice by buffer len
		if outStart > outEnd {
			outStart = outEnd
		}

		if inEnd > inStart {
			srcData.DataIn = input[inStart:inEnd]
		} else {
			srcData.DataIn = nil
		}
		// Always provide an output slice if space exists, Process needs it
		if outEnd > outStart {
			srcData.DataOut = output[outStart:outEnd]
		} else {
			srcData.DataOut = nil
		}
		// If no output space, set OutputFrames to 0
		if outEnd <= outStart {
			srcData.OutputFrames = 0
		}

		srcData.InputFramesUsed = 0
		srcData.OutputFramesGen = 0

		// Call Process
		err = state.Process(&srcData)
		if err != nil {
			t.Fatalf("%s Process() loop %d failed: %v (C Line ~247)", logPrefix, k, err)
		}

		// Check termination
		if srcData.EndOfInput && srcData.OutputFramesGen == 0 {
			totalFramesUsed += srcData.InputFramesUsed // Add final consumption
			totalFramesGen += srcData.OutputFramesGen  // Add final generation (0)
			break
		}

		// Update totals
		totalFramesUsed += srcData.InputFramesUsed
		totalFramesGen += srcData.OutputFramesGen

		// Check stall condition
		if !srcData.EndOfInput && srcData.InputFramesUsed == 0 && srcData.OutputFramesGen == 0 && srcData.InputFrames > 0 {
			t.Fatalf("%s Process() stalled loop %d before EOF. Input provided=%d", logPrefix, k, srcData.InputFrames)
		}

	} // End loop k

	// Assertions from C code (after loop)
	if totalFramesUsed != int64(totalInputFrames) {
		// C didn't assert this, but it's a good check
		t.Errorf("%s Input not fully consumed: Used %d, Expected %d", logPrefix, totalFramesUsed, totalInputFrames)
	}
	if totalFramesGen <= 0 {
		// C asserted > 0
		t.Errorf("%s No output generated (TotalFramesGen = %d)", logPrefix, totalFramesGen)
	}

	// Check for NaNs in output
	outputToCheck := output[:totalFramesGen*int64(channels)] // Slice to actual generated output
	for k, val := range outputToCheck {
		if math.IsNaN(float64(val)) {
			t.Errorf("%s Found NaN in output at index %d", logPrefix, k)
			break // Report first NaN
		}
	}

	// No explicit "ok" log, rely on test passing if no errors/failures
}
