package libsamplerate

import (
	"fmt"
	"math"
	"testing"
	// Assumes types, constants, CallbackNew, CallbackRead, etc. are defined
	// Assumes genWindowedSinesGo, minInt64 are available
)

const (
	varispeedTestInputFrames     = 16384                                             // Smaller than file example, sufficient for test
	varispeedReadChunkFrames     = 128                                               // How many frames to request from CallbackRead at a time
	varispeedCallbackChunkFrames = 64                                                // How many frames the input callback provides
	varispeedMaxOutputFrames     = int(float64(varispeedTestInputFrames)*1.5) + 1000 // Max output based on ratio 1.5
	varispeedMaxRatioFreqPoints  = 20000.0                                           // Denominator from C sin calculation
)

// Data for the input callback
type varispeedCbData struct {
	channels     int
	totalFrames  int64
	currentFrame int64
	data         []float32 // Full interleaved input buffer
}

// Input callback function for varispeed test
func varispeedInputCallbackGo(userData interface{}) (data []float32, framesRead int64, err error) {
	pcbData, ok := userData.(*varispeedCbData)
	if !ok {
		return nil, 0, fmt.Errorf("varispeedInputCallbackGo: invalid userData")
	}

	// Loop input file like C example
	if pcbData.currentFrame >= pcbData.totalFrames {
		pcbData.currentFrame = 0 // Wrap around to beginning
	}

	framesRemaining := pcbData.totalFrames - pcbData.currentFrame
	framesRead = minInt64(int64(varispeedCallbackChunkFrames), framesRemaining)
	if framesRead <= 0 {
		return nil, 0, nil
	} // Should not happen with wrap around unless totalFrames is 0

	startSample := pcbData.currentFrame * int64(pcbData.channels)
	endSample := startSample + framesRead*int64(pcbData.channels)

	if startSample < 0 || endSample > int64(len(pcbData.data)) {
		return nil, 0, fmt.Errorf("varispeedInputCallbackGo: OOB %d:%d len %d", startSample, endSample, len(pcbData.data))
	}

	data = pcbData.data[startSample:endSample]
	pcbData.currentFrame += framesRead

	// fmt.Printf("DEBUG InputCB: Provided %d frames (current=%d)\n", framesRead, pcbData.currentFrame)
	return data, framesRead, nil
}

// TestVarispeedCallbackRatio verifies CallbackRead with varying ratio
func TestVarispeedCallbackRatio(t *testing.T) {

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
		tc := tt // Capture
		if !tc.enabled {
			continue
		}

		t.Run(tc.name, func(t *testing.T) {
			logPrefix := fmt.Sprintf("Varispeed Callback (%s): ", tc.name)
			t.Logf("%s Starting...", logPrefix)

			const channels = 1 // Use mono for simplicity

			// --- Setup ---
			inputBuffer := make([]float32, varispeedTestInputFrames*channels)
			genWindowedSinesGo(1, []float64{0.1}, 0.8, inputBuffer)

			outputBuffer := make([]float32, varispeedMaxOutputFrames*channels)

			cbData := &varispeedCbData{
				channels:     channels,
				totalFrames:  int64(varispeedTestInputFrames),
				currentFrame: 0,
				data:         inputBuffer,
			}

			state, err := CallbackNew(varispeedInputCallbackGo, tc.converter, channels, cbData)
			if err != nil {
				t.Fatalf("%s CallbackNew failed: %v", logPrefix, err)
			}
			defer state.Close()

			// --- Read Loop with Varying Ratio ---
			var freqPoint int = 0
			var totalOutputFrames int64 = 0
			outputWritePos := 0
			maxOutputSamples := len(outputBuffer)
			// Loop until output buffer is reasonably full or error/stall
			for loopCount := 0; outputWritePos < maxOutputSamples-(varispeedReadChunkFrames*channels); loopCount++ {
				if loopCount > (varispeedMaxOutputFrames/varispeedReadChunkFrames)*2+100 { // Safety break
					t.Fatalf("%s Loop seems stuck after %d iterations", logPrefix, loopCount)
				}

				// Calculate varying ratio
				currentRatio := 1.0 - 0.5*math.Sin(float64(freqPoint)*2.0*math.Pi/varispeedMaxRatioFreqPoints)
				freqPoint++

				// Clamp ratio just in case
				if isBadSrcRatio(currentRatio) {
					currentRatio = math.Max(1.0/srcMaxRatio, math.Min(srcMaxRatio, currentRatio))
					t.Logf("%s Clamped ratio to %.5f at freqPoint %d", logPrefix, currentRatio, freqPoint)
				}

				// Determine output slice for CallbackRead
				framesToRead := int64(varispeedReadChunkFrames)
				availableSpace := maxOutputSamples - outputWritePos
				if int(framesToRead)*channels > availableSpace {
					framesToRead = int64(availableSpace / channels)
				}
				if framesToRead <= 0 {
					break
				} // No more output space

				outSlice := outputBuffer[outputWritePos : outputWritePos+int(framesToRead*int64(channels))]

				// Call CallbackRead with varying ratio
				framesReadThisCall, readErr := CallbackRead(state, currentRatio, framesToRead, outSlice)
				if readErr != nil {
					t.Fatalf("%s CallbackRead loop %d failed (ratio %.4f): %v (State Err: %v)", logPrefix, loopCount, currentRatio, readErr, state.LastError())
				}

				if framesReadThisCall <= 0 {
					// This might happen if input callback returns 0 AND internal buffers are empty
					t.Logf("%s CallbackRead returned <= 0 frames on loop %d. Stopping.", logPrefix, loopCount)
					break
				}

				totalOutputFrames += framesReadThisCall
				outputWritePos += int(framesReadThisCall * int64(channels))
			} // End read loop

			// --- Final Checks ---
			finalErr := state.LastError()
			if finalErr != nil {
				t.Errorf("%s Final Error state: %v", logPrefix, finalErr)
			}

			// Check if *some* input was consumed (callback was called)
			if cbData.currentFrame == 0 && varispeedTestInputFrames > 0 {
				t.Errorf("%s Input callback was seemingly never called or consumed no frames", logPrefix)
			}

			// Check if *some* output was generated
			if totalOutputFrames <= 0 {
				t.Errorf("%s Generated zero output frames", logPrefix)
			}

			t.Logf("%s Finished. Consumed input up to frame %d (of %d). Generated %d output frames.", logPrefix, cbData.currentFrame, cbData.totalFrames, totalOutputFrames)

			// Check for NaNs in output (Optional)
			// ...

			if !t.Failed() {
				t.Logf("%s ok", logPrefix)
			}

		}) // End t.Run converter
	} // End loop converters
}
