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
	"testing"
	// Assumes types, constants, New, Process, Close, SetRatio etc. are defined
	// Assumes helpers genWindowedSinesGo, minInt64 etc. are in test_utils.go
)

const (
	// Use sizes appropriate for a test, potentially smaller than C example's file sizes
	timewarpInputLen  = 30000                                     // Total input frames to generate
	timewarpBufferCap = int(float64(timewarpInputLen)*1.5) + 1000 // Capacity for output (1.5x is max ratio in table)
	timewarpStepSize  = 8                                         // Input frames to read/provide per iteration (from C INPUT_STEP_SIZE)
)

// Corresponds to TIMEWARP_FACTOR struct
type timeWarpFactor struct {
	index int64   // Input frame index where ratio should change
	ratio float64 // New ratio to apply from this index onwards
}

// Corresponds to warp[] table in C
var warpData = []timeWarpFactor{
	{0, 1.00000001},
	{20000, 1.01000000},
	{20200, 1.00000001},
	{40000, 1.20000000},
	{40300, 1.00000001},
	{60000, 1.10000000}, // Note: input len is 30000, so these later ones won't be hit
	{60400, 1.00000001},
	{80000, 1.50000000},
	{81000, 1.00000001},
}

// TestTimewarpVariableRatio mimics the logic of timewarp-file.c using Process API
func TestTimewarpVariableRatio(t *testing.T) {

	tests := []struct {
		name      string
		converter ConverterType
		enabled   bool
	}{
		{"ZeroOrderHold", ZeroOrderHold, true},
		{"Linear", Linear, true},
		{"SincFastest", SincFastest, enableSincFastConverter},
		// Add others if desired
	}

	for _, tt := range tests {
		converterTest := tt // Capture
		if !converterTest.enabled {
			t.Logf("Skipping timewarp test for %s (disabled)", converterTest.name)
			continue
		}

		t.Run(converterTest.name, func(t *testing.T) {
			logPrefix := fmt.Sprintf("Timewarp Test (%s): ", converterTest.name)
			t.Logf("%s Starting...", logPrefix)

			// --- Setup ---
			const channels = 1 // C example uses mono logic inside timewarp_convert
			totalInputFrames := int64(timewarpInputLen)

			// Generate input signal
			inputBuffer := make([]float32, totalInputFrames*channels)
			genWindowedSinesGo(1, []float64{0.05}, 0.8, inputBuffer) // Simple sine wave

			// Allocate output buffer
			outputBuffer := make([]float32, timewarpBufferCap*channels)

			// Initialize converter state
			state, err := New(converterTest.converter, channels)
			if err != nil {
				t.Fatalf("%s New failed: %v", logPrefix, err)
			}
			defer state.Close()

			// --- Streaming Loop with Variable Ratio ---
			var inputProvidedTotal int64 = 0   // Frames provided *to* the Process loop
			var inputConsumedTotal int64 = 0   // Frames consumed *by* Process
			var outputGeneratedTotal int64 = 0 // Frames generated *by* Process
			warpIndex := 0
			currentRatio := 1.0 // Default initial ratio

			// Set initial ratio based on warpData[0]
			if len(warpData) > 0 && warpData[0].index == 0 {
				currentRatio = warpData[0].ratio
				err = state.SetRatio(currentRatio)
				if err != nil {
					t.Fatalf("%s Initial SetRatio(%.5f) failed: %v", logPrefix, currentRatio, err)
				}
				warpIndex++
			} else {
				// Set default ratio 1.0 if warpData doesn't start at 0
				err = state.SetRatio(currentRatio)
				if err != nil {
					t.Fatalf("%s Initial SetRatio(1.0) failed: %v", logPrefix, err)
				}
			}

			srcData := SrcData{SrcRatio: currentRatio}              // Init with current ratio
			maxLoops := (totalInputFrames/timewarpStepSize)*2 + 200 // Safety break

			for loopCount := int64(0); ; loopCount++ {
				if loopCount > maxLoops {
					t.Fatalf("%s Loop exceeded max iterations (%d)", logPrefix, maxLoops)
				}

				// 1. Determine input chunk for this iteration
				framesToReadNow := minInt64(int64(timewarpStepSize), totalInputFrames-inputProvidedTotal)
				if framesToReadNow < 0 {
					framesToReadNow = 0
				}

				isEOF := (inputProvidedTotal+framesToReadNow >= totalInputFrames)

				// Prepare input slice
				inStart := inputProvidedTotal * int64(channels)
				inEnd := (inputProvidedTotal + framesToReadNow) * int64(channels)
				if inStart >= int64(len(inputBuffer)) {
					framesToReadNow = 0
				} // Don't read past end
				if inEnd > int64(len(inputBuffer)) {
					inEnd = int64(len(inputBuffer))
					framesToReadNow = (inEnd - inStart) / int64(channels)
				}
				if inStart > inEnd {
					inStart = inEnd
					framesToReadNow = 0
				}

				if framesToReadNow > 0 {
					srcData.DataIn = inputBuffer[inStart:inEnd]
				} else {
					srcData.DataIn = nil // No more input (or zero requested)
				}
				srcData.InputFrames = framesToReadNow
				srcData.EndOfInput = isEOF // Signal EOF on the iteration that provides the last chunk

				// Update total provided BEFORE process call for this chunk
				inputProvidedTotal += framesToReadNow

				// 2. Check and update ratio based on *consumed* frames
				// Check BEFORE processing the current chunk
				for warpIndex < len(warpData) && inputConsumedTotal >= warpData[warpIndex].index {
					currentRatio = warpData[warpIndex].ratio
					err = state.SetRatio(currentRatio)
					if err != nil {
						t.Fatalf("%s SetRatio(%.5f) at index %d failed: %v", logPrefix, currentRatio, inputConsumedTotal, err)
					}
					srcData.SrcRatio = currentRatio // Ensure Process sees the updated ratio
					t.Logf("%s Changed ratio to %.5f at input frame %d", logPrefix, currentRatio, inputConsumedTotal)
					warpIndex++
				}

				// 3. Prepare output slice (provide remaining capacity)
				outSpaceRemaining := int64(len(outputBuffer)) - outputGeneratedTotal*int64(channels)
				outFramesThisBlock := outSpaceRemaining / int64(channels)
				if outFramesThisBlock < 0 {
					outFramesThisBlock = 0
				}

				if outFramesThisBlock == 0 && !isEOF {
					t.Fatalf("%s Ran out of output buffer space before EOF. Output generated: %d", logPrefix, outputGeneratedTotal)
				}

				outStart := outputGeneratedTotal * int64(channels)
				outEnd := outStart + outFramesThisBlock*int64(channels)
				if outEnd > int64(len(outputBuffer)) {
					outEnd = int64(len(outputBuffer))
					outFramesThisBlock = (outEnd - outStart) / int64(channels)
				}
				if outStart > outEnd {
					outStart = outEnd
					outFramesThisBlock = 0
				}

				if outEnd > outStart {
					srcData.DataOut = outputBuffer[outStart:outEnd]
				} else {
					srcData.DataOut = nil
				}
				srcData.OutputFrames = outFramesThisBlock

				// 4. Call Process
				srcData.InputFramesUsed = 0
				srcData.OutputFramesGen = 0

				err = state.Process(&srcData)
				if err != nil {
					t.Fatalf("%s Process() loop %d failed: %v", logPrefix, loopCount, err)
				}

				// 5. Update counters
				inputConsumedTotal += srcData.InputFramesUsed
				outputGeneratedTotal += srcData.OutputFramesGen

				// 6. Check termination
				if srcData.EndOfInput && srcData.OutputFramesGen == 0 {
					t.Logf("%s Process returned EndOfInput and OutputFramesGen=0. Terminating.", logPrefix)
					break
				}

				// Safety check: break if EOF signalled AND no input was provided OR consumed this round
				if isEOF && framesToReadNow == 0 && srcData.InputFramesUsed == 0 {
					t.Logf("%s EOF reached and no input consumed/provided. Terminating.", logPrefix)
					break
				}

			} // End loop

			// --- Final Assertions ---
			if inputConsumedTotal != totalInputFrames {
				t.Errorf("%s Did not consume all input: Consumed %d, Expected %d", logPrefix, inputConsumedTotal, totalInputFrames)
			}
			if outputGeneratedTotal <= 0 {
				t.Errorf("%s Did not generate any output", logPrefix)
			}
			t.Logf("%s Finished. Input consumed=%d, Output generated=%d", logPrefix, inputConsumedTotal, outputGeneratedTotal)

			// Check for NaNs in output (Optional but good)
			outputToCheck := outputBuffer[:outputGeneratedTotal*int64(channels)]
			for k, val := range outputToCheck {
				if math.IsNaN(float64(val)) {
					t.Errorf("%s Found NaN in output at index %d", logPrefix, k)
					break
				}
			}

			if !t.Failed() {
				t.Logf("%s ok", logPrefix)
			}

		}) // End t.Run converter
	} // End loop converters
}
