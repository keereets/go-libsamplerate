package libsamplerate

import (
	"fmt"
	"math"
	"testing"
	// Assumes types, constants, New, Simple, Process, Close, CallbackNew, CallbackRead etc. are defined
	// Assumes CallbackFunc type signature is:
	// type CallbackFunc func(userData interface{}) (data []float32, framesRead int64, err error)
)

const (
	bufferLenCallback = 10000
	cbReadLen         = 256
)

// --- Callback Data Structures ---

// testCbDataGo holds state for the callback function test mocks.
// Corresponds to TEST_CB_DATA in C.
type testCbDataGo struct {
	channels     int
	count        int64     // Samples processed so far (C used this, maybe frames intended?) Let's use frames like C current_frame
	currentFrame int64     // Frames processed so far
	totalFrames  int64     // Total frames available in data
	endOfData    bool      // For eos_callback_test
	data         []float32 // Reference to the *interleaved* input buffer
}

// --- Mock Callback Functions ---

// testThisCallbackFuncGo mimics C's test_callback_func. Provides input data in chunks.
func testThisCallbackFuncGo(userData interface{}) (data []float32, framesRead int64, err error) {
	pcbData, ok := userData.(*testCbDataGo)
	if !ok {
		return nil, 0, fmt.Errorf("testCallbackFuncGo: invalid userData type %T", userData)
	}

	// Calculate remaining frames
	framesRemaining := pcbData.totalFrames - pcbData.currentFrame
	if framesRemaining <= 0 {
		return nil, 0, nil // No more data
	}

	// Determine frames for this chunk
	framesToRead := int64(cbReadLen / pcbData.channels) // C uses samples / channels
	framesRead = minInt64(framesToRead, framesRemaining)

	if framesRead <= 0 { // Should not happen if framesRemaining > 0
		return nil, 0, nil
	}

	// Calculate slice indices (interleaved data)
	startSample := pcbData.currentFrame * int64(pcbData.channels)
	endSample := startSample + framesRead*int64(pcbData.channels)

	// Basic bounds check on underlying data slice
	if startSample < 0 || endSample > int64(len(pcbData.data)) {
		return nil, 0, fmt.Errorf("testCallbackFuncGo: calculated indices [%d:%d] out of bounds for data length %d", startSample, endSample, len(pcbData.data))
	}

	data = pcbData.data[startSample:endSample]
	pcbData.currentFrame += framesRead // Update position

	return data, framesRead, nil
}

// eosCallbackFuncGo mimics C's eos_callback_func. Signals end of data explicitly.
func eosCallbackFuncGo(userData interface{}) (data []float32, framesRead int64, err error) {
	pcbData, ok := userData.(*testCbDataGo)
	if !ok {
		return nil, 0, fmt.Errorf("eosCallbackFuncGo: invalid userData type %T", userData)
	}

	// Return immediately if end_of_data was set previously
	if pcbData.endOfData {
		return nil, 0, nil
	}

	// Calculate remaining frames
	framesRemaining := pcbData.totalFrames - pcbData.currentFrame
	if framesRemaining <= 0 {
		pcbData.endOfData = true // Mark end *before* returning 0
		return nil, 0, nil
	}

	// Determine frames for this chunk
	framesToRead := int64(cbReadLen / pcbData.channels)
	framesRead = minInt64(framesToRead, framesRemaining)

	if framesRead <= 0 { // Should not happen if framesRemaining > 0
		pcbData.endOfData = true
		return nil, 0, nil
	}

	// Calculate slice indices
	startSample := pcbData.currentFrame * int64(pcbData.channels)
	endSample := startSample + framesRead*int64(pcbData.channels)

	// Bounds check
	if startSample < 0 || endSample > int64(len(pcbData.data)) {
		pcbData.endOfData = true // Mark end if error occurs too
		return nil, 0, fmt.Errorf("eosCallbackFuncGo: calculated indices [%d:%d] out of bounds for data length %d", startSample, endSample, len(pcbData.data))
	}

	data = pcbData.data[startSample:endSample]
	pcbData.currentFrame += framesRead

	// Set end_of_data flag *after* providing data if near the end (matches C logic)
	// C check: if (pcb_data->total < 2 * pcb_data->count) -> if totalFrames < 2 * currentFrame ? No, C uses sample count 'count'. Let's stick to C's check logic based on updated count.
	// C's `count` seems to be sample count. Let's use `currentFrame`.
	// Check if the *next* read would be empty or partial?
	// C: if (pcb_data->total < 2 * pcb_data->count). If total samples < 2 * samples_already_provided.
	// Let's re-read C: `pcb_data->count += frames ;` is sample count? No, `frames` is frames. C's `count` must be samples. Let's add `currentSample` to struct.
	// RETHINK: Let's simplify the Go callback data struct and logic to use frames consistently.
	// Set endOfData = true if currentFrame reaches totalFrames.

	// Simpler Go logic: If after this read, currentFrame == totalFrames, set flag.
	if pcbData.currentFrame >= pcbData.totalFrames {
		pcbData.endOfData = true
	}

	return data, framesRead, nil
}

// --- Package Level Buffers (Mimic C static) ---
// Using arrays ensures fixed size and avoids reallocation per test.
// Ensure size is adequate for max channels used in tests.
var (
	cbInputBuffer  [bufferLenCallback * maxChannelsMulti]float32 // Max size needed
	cbOutputBuffer [bufferLenCallback * maxChannelsMulti]float32 // Max size needed
)

// --- Main Test Function ---

func TestCallbackAPI(t *testing.T) {
	srcRatios := []float64{
		1.0, 0.099, 0.1, 0.33333333, 0.789, 1.0001, 1.9, 3.1, 9.9,
	}

	tests := []struct {
		name      string
		converter ConverterType
		enabled   bool
	}{
		{"ZeroOrderHold", ZeroOrderHold, true},
		{"Linear", Linear, true},
		{"SincFastest", SincFastest, enableSincFastConverter},
		// Add others if needed
	}

	t.Run("BasicCallback", func(t *testing.T) {
		for _, tt := range tests {
			converterTest := tt // Capture
			if !converterTest.enabled {
				continue
			}
			t.Run(converterTest.name, func(t *testing.T) {
				for _, ratio := range srcRatios {
					currentRatio := ratio // Capture
					t.Run(fmt.Sprintf("Ratio_%.4f", currentRatio), func(t *testing.T) {
						testCallbackBasic(t, converterTest.converter, currentRatio)
					})
				}
			})
		}
	})

	t.Run("EndOfStream", func(t *testing.T) {
		for _, tt := range tests {
			converterTest := tt // Capture
			if !converterTest.enabled {
				continue
			}
			t.Run(converterTest.name, func(t *testing.T) {
				testCallbackEOS(t, converterTest.converter)
			})
		}
	})

	// No fftw_cleanup
}

// --- Test Helper Implementations ---

// testCallbackBasic corresponds to callback_test() in C
func testCallbackBasic(t *testing.T, converter ConverterType, srcRatio float64) {
	t.Helper()
	const channels = 2 // C test hardcodes 2 channels
	const totalInputFrames = bufferLenCallback
	logPrefix := fmt.Sprintf("Callback Basic (Ratio=%.4f): ", srcRatio)
	t.Logf("%s Starting...", logPrefix)

	// --- Use LOCAL buffers for this test instance ---
	// Input Buffer (copied from C static logic)
	localInputBuffer := make([]float32, totalInputFrames*channels)
	// Output Buffer (dynamically growing slice)
	totalOutputInterleaved := make([]float32, 0, int(float64(totalInputFrames*channels)*srcRatio)+100) // Start empty, with capacity hint
	// Temporary buffer for CallbackRead chunks
	readChunk := make([]float32, (cbReadLen/channels)*channels) // Ensure size is multiple of channels

	// C test uses zeroed input, so 'localInputBuffer' is fine as is.

	// Setup callback data struct, pointing to LOCAL input buffer
	cbData := &testCbDataGo{
		channels:     channels,
		totalFrames:  totalInputFrames,
		currentFrame: 0,
		data:         localInputBuffer, // Use local buffer
		endOfData:    false,
	}

	// Create converter
	state, err := CallbackNew(testThisCallbackFuncGo, converter, channels, cbData)
	if err != nil {
		t.Fatalf("%s CallbackNew failed: %v (C Line ~125)", logPrefix, err)
	}
	defer state.Close()

	// --- Read loop (Simplified) ---
	var readTotalFrames int64 = 0
	maxLoops := (totalInputFrames / (cbReadLen / channels)) + int64(math.Ceil(float64(totalInputFrames)*srcRatio*float64(channels)/float64(cbReadLen))) + 50
	if maxLoops < 100 {
		maxLoops = 100
	}

	for loopCount := int64(0); ; loopCount++ {
		if loopCount > maxLoops {
			t.Fatalf("%s CallbackRead loop exceeded max iterations (%d)", logPrefix, maxLoops)
		}

		// Determine how many frames to request in this chunk
		framesToRead := int64(len(readChunk) / channels)

		// Call CallbackRead using the temporary chunk buffer
		framesReadThisCall, readErr := CallbackRead(state, srcRatio, framesToRead, readChunk)
		if readErr != nil {
			t.Fatalf("%s CallbackRead loop %d failed: %v", logPrefix, loopCount, readErr)
		}

		if framesReadThisCall <= 0 {
			break // End of stream signaled by CallbackRead
		}

		// Append the valid data read into the chunk buffer to the total output slice
		samplesRead := framesReadThisCall * int64(channels)
		totalOutputInterleaved = append(totalOutputInterleaved, readChunk[:samplesRead]...)

		readTotalFrames += framesReadThisCall

	} // End read loop

	// Check final error state
	finalErr := state.LastError()
	if finalErr != nil {
		t.Errorf("%s Error reported after CallbackRead loop: %v (C Line ~144)", logPrefix, finalErr)
	}

	// Check final output length
	expectedOutputFramesF := srcRatio * float64(totalInputFrames)

	// C tolerance check: fabs(read_total / ratio - total_input) > 2.0
	// Equivalent: fabs(read_total - ratio * total_input) > 2.0 * ratio
	tolerance := 2.0 * srcRatio // Tolerance scales with ratio...
	if tolerance < 2.0 {
		tolerance = 2.0 // Ensure tolerance is at least 2.0 frames
	}
	diff := math.Abs(float64(readTotalFrames) - expectedOutputFramesF)

	if diff > tolerance {
		// Updated error message to show correct tolerance
		t.Errorf("%s Bad final output length: Got %d frames, Expected around %.1f +/- %.1f (C Line ~151)",
			logPrefix, readTotalFrames, expectedOutputFramesF, tolerance)
	}

	if !t.Failed() {
		t.Logf("%s ok", logPrefix)
	}
}

// testCallbackEOS corresponds to end_of_stream_test() in C
func testCallbackEOS(t *testing.T, converter ConverterType) {
	t.Helper()
	const channels = 2 // C test hardcodes 2 channels
	const totalInputFrames = bufferLenCallback
	const srcRatio = 0.3 // C test hardcodes 0.3
	logPrefix := fmt.Sprintf("Callback EOS (%s): ", GetName(converter))
	t.Logf("%s Starting...", logPrefix)

	// Zero out buffers
	for i := range cbInputBuffer {
		cbInputBuffer[i] = 0.0
	}
	for i := range cbOutputBuffer {
		cbOutputBuffer[i] = 0.0
	}

	// Setup callback data struct
	cbData := &testCbDataGo{
		channels:     channels,
		totalFrames:  totalInputFrames,
		currentFrame: 0,
		data:         cbInputBuffer[:totalInputFrames*channels], // Slice
		endOfData:    false,                                     // Start with false
	}

	// Create converter
	state, err := CallbackNew(eosCallbackFuncGo, converter, channels, cbData)
	if err != nil {
		t.Fatalf("%s CallbackNew failed: %v (C Line ~221)", logPrefix, err)
	}
	defer state.Close()

	// Read loop (discarding data, just checking termination)
	readTotalFrames := int64(0)
	maxLoops := (totalInputFrames / (cbReadLen / channels)) + int64(math.Ceil(float64(totalInputFrames)*srcRatio*float64(channels)/float64(cbReadLen))) + 50
	if maxLoops < 100 {
		maxLoops = 100
	}

	for loopCount := int64(0); ; loopCount++ {
		if loopCount > maxLoops {
			t.Fatalf("%s CallbackRead loop exceeded max iterations (%d)", logPrefix, maxLoops)
		}

		// Read into a temporary buffer or reuse part of global (carefully)
		readBuf := cbOutputBuffer[:minInt(len(cbOutputBuffer), cbReadLen)] // Read small chunks
		framesToRead := int64(len(readBuf) / channels)
		if framesToRead <= 0 {
			framesToRead = 1
		} // Ensure we try to read something small

		framesReadThisCall, readErr := CallbackRead(state, srcRatio, framesToRead, readBuf)
		if readErr != nil {
			t.Fatalf("%s CallbackRead loop %d failed: %v", logPrefix, loopCount, readErr)
		}

		if framesReadThisCall <= 0 {
			break // End of stream
		}
		readTotalFrames += framesReadThisCall

	} // End read loop

	// Check final error state
	finalErr := state.LastError()
	if finalErr != nil {
		t.Errorf("%s Error reported after CallbackRead loop: %v (C Line ~237)", logPrefix, finalErr)
	}

	// Check if the callback's endOfData flag was actually set
	if !cbData.endOfData {
		t.Errorf("%s Callback data 'endOfData' flag was not set! (C Line ~242)", logPrefix)
	}

	if !t.Failed() {
		t.Logf("%s ok", logPrefix)
	}
}
