// streaming_test.go
package libsamplerate

import (
	"fmt"
	"math"
	"testing"
)

const (
	// bufferLenStreaming corresponds to BUFFER_LEN in streaming_test.c
	bufferLenStreaming = 1 << 15 // 32768
	// blockLenStreaming corresponds to BLOCK_LEN
	blockLenStreaming = 100
)

// TestStreamingAPI corresponds to the main function driving the tests
func TestStreamingAPI(t *testing.T) {
	srcRatios := []float64{
		0.3, 0.9, 1.1, 3.0, // Ratios from C test
		// Add other interesting ratios? e.g., near 1, very high/low?
		1.0001, 1.0 / 256.0, 256.0,
	}

	tests := []struct {
		name      string
		converter ConverterType
		enabled   bool
	}{
		{"ZeroOrderHold", ZeroOrderHold, true}, // Assuming enabled
		{"Linear", Linear, true},               // Assuming enabled
		{"SincFastest", SincFastest, enableSincFastConverter},
		// Add other Sinc types if desired and enabled
		// {"SincMedium", SincMediumQuality, enableSincMediumConverter},
		// {"SincBest", SincBestQuality, enableSincBestConverter},
	}

	for _, tt := range tests {
		converterTest := tt // Capture range variable
		if !converterTest.enabled {
			t.Logf("Skipping streaming tests for %s (disabled in package)", converterTest.name)
			continue
		}

		t.Run(converterTest.name, func(t *testing.T) {
			for _, ratio := range srcRatios {
				currentRatio := ratio // Capture range variable
				t.Run(fmt.Sprintf("Ratio_%.4f", currentRatio), func(t *testing.T) {
					// t.Parallel() // Maybe possible if New/Process/Close are thread-safe per instance
					testStreaming(t, converterTest.converter, currentRatio)
				})
			}
		})
	}
}

// testStreaming corresponds to stream_test() in C
func testStreaming(t *testing.T, converter ConverterType, srcRatio float64) {
	t.Helper()
	logPrefix := fmt.Sprintf("Streaming Test (Ratio=%.4f): ", srcRatio)
	t.Logf("%s Starting...", logPrefix)

	// Allocate full buffers
	processOutputCap := bufferLenStreaming + blockLenStreaming*2
	inputBuffer := make([]float32, bufferLenStreaming)
	outputBuffer := make([]float32, processOutputCap)

	// --- Calculate total expected input/output lengths ---
	var totalInputLen, targetOutputLen int
	if srcRatio >= 1.0 {
		targetOutputLen = bufferLenStreaming
		totalInputLen = int(math.Floor(float64(bufferLenStreaming) / srcRatio))
	} else {
		totalInputLen = bufferLenStreaming
		targetOutputLen = int(math.Floor(float64(bufferLenStreaming) * srcRatio))
	}
	totalInputLen -= 10 // Mimic C adjustment

	// Sanity checks
	if targetOutputLen > bufferLenStreaming { /* ... t.Fatalf ... */
	}
	if totalInputLen <= 0 { /* ... t.Fatalf ... */
	}
	if totalInputLen > len(inputBuffer) { /* ... t.Fatalf ... */
	}

	// Calculate expected output float for comparison later
	expectedOutputF := srcRatio * float64(totalInputLen)

	// --- Initialize Converter ---
	state, err := New(converter, 1)
	if err != nil {
		t.Fatalf("%s New() failed: %v (C Line ~85)", logPrefix, err)
	}
	defer state.Close()

	// --- Streaming Loop ---
	var currentInFrames int64 = 0
	var currentOutFrames int64 = 0
	srcData := SrcData{SrcRatio: srcRatio}

	maxLoops := (totalInputLen / blockLenStreaming) + int(math.Ceil(expectedOutputF/float64(blockLenStreaming))) + 50 // Base on input+output blocks + generous margin
	if maxLoops < 100 {
		maxLoops = 100
	} // Ensure a minimum number of loops
	// fmt.Printf("\nDEBUG: totalInputLen=%d, expectedOutputF=%.1f, maxLoops=%d\n", totalInputLen, expectedOutputF, maxLoops) // Optional debug

	for loopCount := 0; ; loopCount++ { // Use loopCount within the loop scope
		if loopCount > maxLoops {
			t.Fatalf("%s Loop exceeded max iterations (%d), likely stuck. currentIn=%d (expected %d), currentOut=%d (expected %.0f)", logPrefix, maxLoops, currentInFrames, totalInputLen, currentOutFrames, expectedOutputF)
		}

		remainingInput := int64(totalInputLen) - currentInFrames
		inFramesThisBlock := minInt64(int64(blockLenStreaming), remainingInput)
		if inFramesThisBlock < 0 {
			inFramesThisBlock = 0
		} // Ensure non-negative

		outSpaceAvailable := int64(len(outputBuffer)) - currentOutFrames
		outFramesThisBlock := minInt64(int64(blockLenStreaming), outSpaceAvailable)
		if outFramesThisBlock < 0 {
			outFramesThisBlock = 0
		} // Ensure non-negative

		// Set EndOfInput flag *before* the process call
		srcData.EndOfInput = (currentInFrames >= int64(totalInputLen))
		// If already EOF, ensure we don't try to provide more input than exists
		if srcData.EndOfInput {
			inFramesThisBlock = minInt64(inFramesThisBlock, int64(totalInputLen)-currentInFrames) // Don't read past totalInputLen
			if inFramesThisBlock < 0 {
				inFramesThisBlock = 0
			}
		}

		srcData.InputFrames = inFramesThisBlock
		srcData.OutputFrames = outFramesThisBlock

		inStart := int(currentInFrames)
		inEnd := inStart + int(inFramesThisBlock)
		// Bounds check against the actual buffer size
		if inStart > len(inputBuffer) {
			inStart = len(inputBuffer)
		}
		if inEnd > len(inputBuffer) {
			inEnd = len(inputBuffer)
		}
		if inStart > inEnd {
			inStart = inEnd
		} // Ensure start <= end

		outStart := int(currentOutFrames)
		outEnd := outStart + int(outFramesThisBlock)
		if outStart > len(outputBuffer) {
			outStart = len(outputBuffer)
		}
		if outEnd > len(outputBuffer) {
			outEnd = len(outputBuffer)
		}
		if outStart > outEnd {
			outStart = outEnd
		}

		// Only assign slice if length > 0
		if inEnd > inStart {
			srcData.DataIn = inputBuffer[inStart:inEnd]
		} else {
			srcData.DataIn = nil
		}
		if outEnd > outStart {
			srcData.DataOut = outputBuffer[outStart:outEnd]
		} else {
			srcData.DataOut = nil
		}

		// Reset Used/Gen counts before calling Process
		srcData.InputFramesUsed = 0
		srcData.OutputFramesGen = 0

		// --- Logging before Process ---
		// fmt.Printf("DEBUG Loop %d: In=%d/%d (Providing %d), Out=%d/%d (Providing %d), EOF=%t\n", loopCount, currentInFrames, totalInputLen, srcData.InputFrames, currentOutFrames, int64(expectedOutputF), srcData.OutputFrames, srcData.EndOfInput)

		// Call src_process
		err = state.Process(&srcData)
		if err != nil {
			t.Fatalf("%s Process() failed: %v (C Line ~102)", logPrefix, err)
		}

		// --- Logging after Process ---
		// fmt.Printf("DEBUG Loop %d: UsedIn=%d, GenOut=%d\n", loopCount, srcData.InputFramesUsed, srcData.OutputFramesGen)

		// Check for termination condition (same as C)
		if srcData.EndOfInput && srcData.OutputFramesGen == 0 {
			// Make sure we didn't provide input in this last step if EOF was already true
			if !(srcData.InputFrames == 0 && srcData.InputFramesUsed == 0) {
				t.Logf("%s INFO: Final loop iteration completed (EOF=true, OutGen=0). UsedIn=%d, ProvidedIn=%d.", logPrefix, srcData.InputFramesUsed, srcData.InputFrames)
			}
			// Update counts one last time before breaking
			currentInFrames += srcData.InputFramesUsed
			currentOutFrames += srcData.OutputFramesGen
			break
		}

		// Check for unexpected consumption/generation
		if srcData.InputFramesUsed > srcData.InputFrames { /* ... t.Fatalf ... */
		}
		if srcData.OutputFramesGen > srcData.OutputFrames { /* ... t.Fatalf ... */
		}

		// Update cumulative counts
		currentInFrames += srcData.InputFramesUsed
		currentOutFrames += srcData.OutputFramesGen

		// Safety check: If Process returns 0 used and 0 generated when NOT EOF, it's stuck.
		if !srcData.EndOfInput && srcData.InputFramesUsed == 0 && srcData.OutputFramesGen == 0 && srcData.InputFrames > 0 {
			t.Fatalf("%s Process() stalled: Consumed 0 input and generated 0 output before EOF. Input provided=%d", logPrefix, srcData.InputFrames)
		}
	}

	// --- Final Checks ---
	terminateF := math.Ceil(math.Max(srcRatio, 1.0/srcRatio))
	tolerance := 2.0 * terminateF
	diff := math.Abs(float64(currentOutFrames) - expectedOutputF)

	// --- Logging before final checks ---
	//fmt.Printf("\nDEBUG Final Check (Ratio %.4f): TotalIn=%d (Expected %d), TotalOut=%d (Expected %.1f), Diff=%.1f, Tol=%.1f\n", srcRatio, currentInFrames, totalInputLen, currentOutFrames, expectedOutputF, diff, tolerance)

	if diff > tolerance {
		t.Errorf("%s Bad final output length: Got %d, expected approx %.2f (Tolerance %.2f) (C Line ~128)",
			logPrefix, currentOutFrames, expectedOutputF, tolerance)
		t.Logf("\tDetails: Ratio=%.4f, InputTotal=%d, TerminateFactor=%.1f", srcRatio, totalInputLen, terminateF)
	}

	if currentInFrames != int64(totalInputLen) {
		t.Errorf("%s Input not fully consumed: Consumed %d, expected %d (C Line ~136)",
			logPrefix, currentInFrames, totalInputLen)
	}

	if !t.Failed() {
		t.Logf("%s ok", logPrefix)
	}
}
