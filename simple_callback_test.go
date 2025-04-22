//
// Copyright (c) 2025, Antonio Chirizzi <antonio.chirizzi@gmail.com>
// All rights reserved.
//
// This code is released under 3-clause BSD license. Please see the
// file LICENSE
//

package libsamplerate // Assuming tests are in the same package

import (
	"fmt"
	"math"
	"testing"
	// Assumes types, constants, New, CallbackNew, CallbackRead etc. are defined
)

// --- Simple Callback Data & Function ---

const simpleInputLen = 10 // Very small input frames
const simpleChunkLen = 4  // Small callback chunk size (frames)
const simpleChannels = 1  // Mono

type simpleCbData struct {
	totalFrames  int64
	currentFrame int64
	data         []float32 // Simple input data
}

// simpleCallbackFunc returns simple chunks of data (all 1.0s)
func simpleCallbackFunc(userData interface{}) (data []float32, framesRead int64, err error) {
	pcbData, ok := userData.(*simpleCbData)
	if !ok {
		return nil, 0, fmt.Errorf("simpleCallbackFunc: invalid userData type %T", userData)
	}

	framesRemaining := pcbData.totalFrames - pcbData.currentFrame
	if framesRemaining <= 0 {
		fmt.Println("DEBUG SimpleCallback: Returning 0 frames (EOF)")
		return nil, 0, nil // No more data
	}

	framesRead = minInt64(int64(simpleChunkLen), framesRemaining)
	if framesRead <= 0 {
		return nil, 0, nil
	}

	startSample := pcbData.currentFrame * int64(simpleChannels)
	endSample := startSample + framesRead*int64(simpleChannels)

	if startSample < 0 || endSample > int64(len(pcbData.data)) {
		return nil, 0, fmt.Errorf("simpleCallbackFunc: calculated indices [%d:%d] out of bounds for data length %d", startSample, endSample, len(pcbData.data))
	}

	data = pcbData.data[startSample:endSample]
	pcbData.currentFrame += framesRead

	fmt.Printf("DEBUG SimpleCallback: Returning %d frames (indices %d:%d)\n", framesRead, startSample, endSample)
	return data, framesRead, nil
}

// --- Simplified Test Function ---

func TestCallbackSimpleMinimal(t *testing.T) {
	// Use ratio that previously failed (downsampling)
	const testRatio = 0.0990 // Ratio from failing test
	// Use simple ZOH converter
	const converter = ZeroOrderHold

	t.Logf("Running TestCallbackSimpleMinimal: Ratio=%.4f, InputLen=%d, ChunkLen=%d", testRatio, simpleInputLen, simpleChunkLen)

	// --- Setup ---
	// Create simple input data (all 1.0s)
	inputData := make([]float32, simpleInputLen*simpleChannels)
	for i := range inputData {
		inputData[i] = 1.0
	}

	// Callback data struct
	cbData := &simpleCbData{
		//channels:     simpleChannels,
		totalFrames:  int64(simpleInputLen),
		currentFrame: 0,
		data:         inputData,
	}

	// Create converter using callback
	state, err := CallbackNew(simpleCallbackFunc, converter, simpleChannels, cbData)
	if err != nil {
		t.Fatalf("CallbackNew failed: %v", err)
	}
	defer state.Close()

	// --- Read Loop ---
	// Estimate total output and create buffer. Ratio ~0.1, Input=10 -> Output ~1 frame. Let's make buffer bigger.
	outputBufferSize := 50 // Frames
	outputBuffer := make([]float32, outputBufferSize*simpleChannels)
	var readTotalFrames int64 = 0
	outputWritePos := 0

	for loopCount := 0; loopCount < 10; loopCount++ { // Limit loops for safety

		// How many frames to request this time? Let's ask for a small fixed amount.
		framesToRead := int64(10) // Request 10 output frames
		if outputWritePos+int(framesToRead*int64(simpleChannels)) > len(outputBuffer) {
			framesCanWrite := (len(outputBuffer) - outputWritePos) / simpleChannels
			if framesCanWrite <= 0 {
				t.Logf("Output buffer full. Breaking read loop.")
				break
			}
			framesToRead = int64(framesCanWrite)
		}
		if framesToRead <= 0 {
			break
		} // Safety break

		outSlice := outputBuffer[outputWritePos : outputWritePos+int(framesToRead*int64(simpleChannels))]
		t.Logf("Loop %d: Calling CallbackRead (Ratio=%.3f, Read=%d frames)", loopCount, testRatio, framesToRead)

		framesReadThisCall, readErr := CallbackRead(state, testRatio, framesToRead, outSlice)

		t.Logf("Loop %d: CallbackRead returned %d frames, err=%v", loopCount, framesReadThisCall, readErr)

		if readErr != nil {
			// Include state error if available
			t.Fatalf("CallbackRead loop %d failed: %v (State Err: %v)", loopCount, readErr, state.LastError())
		}

		if framesReadThisCall <= 0 {
			t.Logf("Loop %d: CallbackRead returned <= 0 frames. Assuming EOF.", loopCount)
			break // End of stream signaled
		}

		readTotalFrames += framesReadThisCall
		outputWritePos += int(framesReadThisCall * int64(simpleChannels))

	} // End read loop

	// --- Final Checks ---
	finalErr := state.LastError()
	if finalErr != nil {
		t.Errorf("Final Error state: %v", finalErr)
	}

	// Check how much input was actually requested by CallbackRead via the callback
	if cbData.currentFrame != cbData.totalFrames {
		t.Errorf("Callback did not consume all input: Consumed %d frames, Total %d frames", cbData.currentFrame, cbData.totalFrames)
	}

	// Check output frames (very rough check for this simple test)
	expectedOutputFramesRough := int64(math.Floor(float64(cbData.totalFrames) * testRatio)) // Floor for downsampling
	t.Logf("Total output frames generated: %d (Expected roughly: %d)", readTotalFrames, expectedOutputFramesRough)
	if readTotalFrames == 0 && cbData.totalFrames > 0 {
		t.Errorf("Generated zero output frames from non-zero input")
	}
	// Add more specific checks if needed, e.g., on outputBuffer content

	t.Logf("TestCallbackSimpleMinimal finished.")
}

// TestCallbackSimpleUpsample uses a simple setup but with an upsampling ratio.
func TestCallbackSimpleUpsample(t *testing.T) {
	// Use ratio > 1.0 (e.g., one that failed in InitTerm before tolerance change)
	const testRatio = 3.1
	const converter = ZeroOrderHold // Keep converter simple for now

	t.Logf("Running TestCallbackSimpleUpsample: Ratio=%.4f, InputLen=%d, ChunkLen=%d", testRatio, simpleInputLen, simpleChunkLen)

	// --- Setup ---
	inputData := make([]float32, simpleInputLen*simpleChannels)
	for i := range inputData {
		inputData[i] = 1.0
	}

	cbData := &simpleCbData{
		// channels field removed
		totalFrames:  int64(simpleInputLen),
		currentFrame: 0,
		data:         inputData,
	}

	state, err := CallbackNew(simpleCallbackFunc, converter, simpleChannels, cbData)
	if err != nil {
		t.Fatalf("CallbackNew failed: %v", err)
	}
	defer state.Close()

	// --- Read Loop ---
	// Estimate total output and create buffer. Ratio ~3.1, Input=10 -> Output ~31 frames.
	outputBufferSize := 100 // Increase buffer size significantly for upsampling
	outputBuffer := make([]float32, outputBufferSize*simpleChannels)
	var readTotalFrames int64 = 0
	outputWritePos := 0
	// Adjust maxLoops estimate for upsampling
	maxLoops := int64(simpleInputLen/simpleChunkLen) + int64(math.Ceil(float64(outputBufferSize)/float64(simpleChunkLen))) + 50

	for loopCount := int64(0); ; loopCount++ {
		if loopCount > maxLoops {
			t.Fatalf("CallbackRead loop exceeded max iterations (%d)", maxLoops)
		}

		// How many frames to request? Ask for more output frames now due to upsampling.
		framesToRead := int64(30) // Request more output frames
		if outputWritePos+int(framesToRead*int64(simpleChannels)) > len(outputBuffer) {
			framesCanWrite := (len(outputBuffer) - outputWritePos) / simpleChannels
			if framesCanWrite <= 0 {
				t.Logf("Output buffer full. Breaking read loop.")
				break
			}
			framesToRead = int64(framesCanWrite)
		}
		if framesToRead <= 0 {
			break
		}

		outSlice := outputBuffer[outputWritePos : outputWritePos+int(framesToRead*int64(simpleChannels))]
		t.Logf("Loop %d: Calling CallbackRead (Ratio=%.3f, Read=%d frames)", loopCount, testRatio, framesToRead)

		framesReadThisCall, readErr := CallbackRead(state, testRatio, framesToRead, outSlice)

		t.Logf("Loop %d: CallbackRead returned %d frames, err=%v", loopCount, framesReadThisCall, readErr)

		if readErr != nil {
			t.Fatalf("CallbackRead loop %d failed: %v (State Err: %v)", loopCount, readErr, state.LastError())
		}

		if framesReadThisCall <= 0 {
			t.Logf("Loop %d: CallbackRead returned <= 0 frames. Assuming EOF.", loopCount)
			break // End of stream signaled
		}

		readTotalFrames += framesReadThisCall
		outputWritePos += int(framesReadThisCall * int64(simpleChannels))

	} // End read loop

	// --- Final Checks ---
	finalErr := state.LastError()
	if finalErr != nil {
		t.Errorf("Final Error state: %v", finalErr)
	}

	if cbData.currentFrame != cbData.totalFrames {
		t.Errorf("Callback did not consume all input: Consumed %d frames, Total %d frames", cbData.currentFrame, cbData.totalFrames)
	}

	expectedOutputFramesRough := int64(math.Floor(float64(cbData.totalFrames) * testRatio)) // Use Floor for comparison consistency maybe? C used Floor too.
	t.Logf("Total output frames generated: %d (Expected roughly: %d)", readTotalFrames, expectedOutputFramesRough)
	// Check if output is reasonably close (e.g., within ceil(ratio)+1 like termination test)
	tolerance := int64(math.Ceil(testRatio)) + 1
	if math.Abs(float64(readTotalFrames-expectedOutputFramesRough)) > float64(tolerance) {
		t.Errorf("Output frame count differs significantly from expectation: Got %d, Expected ~%d +/- %d", readTotalFrames, expectedOutputFramesRough, tolerance)
	}
	if readTotalFrames == 0 && cbData.totalFrames > 0 {
		t.Errorf("Generated zero output frames from non-zero input")
	}

	t.Logf("TestCallbackSimpleUpsample finished.")
}
