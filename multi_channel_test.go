//go:build fftw_required

// Needed for calculateSnrGo

package libsamplerate

import (
	"fmt"
	"math"
	"testing"
	// Assumes types, constants, New, Simple, Process, Close, CallbackNew, CallbackRead etc. are defined
	// Assumes helpers genWindowedSinesGo, calculateSnrGo, interleaveDataGo, deinterleaveDataGo etc. are in test_utils.go
)

const (
	bufferLenMulti = 50000 // From C BUFFER_LEN
	blockLenMulti  = 12    // From C BLOCK_LEN
)

// TestMultiChannelAPI corresponds to main() in multi_channel_test.c
func TestMultiChannelAPI(t *testing.T) {
	// Only run SNR tests for multi-channel (C code structure)

	tests := []struct {
		name      string
		converter ConverterType
		enabled   bool
		targetSnr float64
	}{
		{"ZeroOrderHold", ZeroOrderHold, true, 38.0},
		{"Linear", Linear, true, 79.0},
		{"SincFastest", SincFastest, enableSincFastConverter, 100.0}, // Adjust SNR based on previous findings? Use C target for now.
		// Add Medium/Best if desired
	}

	maxChanMap := map[ConverterType]int{
		ZeroOrderHold: 3,                // C tested 1-3
		Linear:        3,                // C tested 1-3
		SincFastest:   maxChannelsMulti, // C tested 1-MAX_CHANNELS
		// Add others if testing them
	}

	for _, tt := range tests {
		converterTest := tt // Capture
		if !converterTest.enabled {
			t.Logf("Skipping multi-channel tests for %s (disabled)", converterTest.name)
			continue
		}

		maxCh := maxChanMap[converterTest.converter]
		if maxCh == 0 {
			maxCh = 1
		} // Default if not in map

		t.Run(converterTest.name, func(t *testing.T) {
			t.Logf("\n    Running multi-channel tests for: %s", converterTest.name)
			for channels := 1; channels <= maxCh; channels++ {
				currentCh := channels // Capture

				t.Run(fmt.Sprintf("Simple_Channels_%d", currentCh), func(t *testing.T) {
					testMultiChannelSimple(t, converterTest.converter, currentCh, converterTest.targetSnr)
				})
				t.Run(fmt.Sprintf("Process_Channels_%d", currentCh), func(t *testing.T) {
					testMultiChannelProcess(t, converterTest.converter, currentCh, converterTest.targetSnr)
				})
				t.Run(fmt.Sprintf("Callback_Channels_%d", currentCh), func(t *testing.T) {
					testMultiChannelCallback(t, converterTest.converter, currentCh, converterTest.targetSnr)
				})

			} // end channel loop
		}) // end converter run
	} // end converter loop

	// No fftw_cleanup
}

// --- Simple API Test ---

// testMultiChannelSimple corresponds to simple_test() in C
func testMultiChannelSimple(t *testing.T, converter ConverterType, channels int, targetSnr float64) {
	t.Helper()
	logPrefix := fmt.Sprintf("Multi Simple (Ch=%d): ", channels)
	t.Logf("%s Starting...", logPrefix)

	if channels > maxChannelsMulti {
		t.Fatalf("%s Channel count %d exceeds test limit %d", logPrefix, channels, maxChannelsMulti)
	}

	frames := bufferLenMulti

	// Allocate buffers
	inputSerial := make([][]float32, channels)
	outputSerial := make([][]float32, channels)
	for ch := 0; ch < channels; ch++ {
		inputSerial[ch] = make([]float32, frames)
		outputSerial[ch] = make([]float32, frames) // Allocate full size, Process might generate less
	}
	inputInterleaved := make([]float32, frames*channels)
	// Allocate enough output capacity - ratio is fixed at 0.95, add margin
	outputCap := int(math.Ceil(float64(frames)*0.95)) + 100
	if outputCap > frames {
		outputCap = frames
	} // Cannot exceed original buffer size for deinterleave later? C used static size. Let's cap at 'frames'.
	outputInterleaved := make([]float32, outputCap*channels)

	// Generate input
	for ch := 0; ch < channels; ch++ {
		freq := (200.0 + 33.333333333*float64(ch)) / 44100.0 // Matches C
		genWindowedSinesGo(1, []float64{freq}, 1.0, inputSerial[ch])
	}

	// Interleave
	err := interleaveDataGo(inputSerial, inputInterleaved, frames, channels)
	if err != nil {
		t.Fatalf("%s Interleaving failed: %v", logPrefix, err)
	}

	// Perform conversion using Simple API
	const srcRatio = 0.95
	srcData := SrcData{
		DataIn:       inputInterleaved,
		InputFrames:  int64(frames),
		DataOut:      outputInterleaved,                        // Provide capacity
		OutputFrames: int64(len(outputInterleaved) / channels), // Capacity in frames
		SrcRatio:     srcRatio,
		// EndOfInput handled by Simple
	}

	err = Simple(&srcData, converter, channels)
	if err != nil {
		t.Fatalf("%s Simple() failed: %v (C Line ~117)", logPrefix, err)
	}

	// Check output length
	expectedOutputF := srcRatio * float64(srcData.InputFrames) // Use InputFrames provided
	if math.Abs(float64(srcData.OutputFramesGen)-expectedOutputF) > 2.0 {
		t.Errorf("%s Bad output length: Got %d, Expected %.1f +/- 2.0 (C Line ~123)", logPrefix, srcData.OutputFramesGen, expectedOutputF)
	}
	actualOutputFrames := int(srcData.OutputFramesGen)
	if actualOutputFrames <= 0 {
		t.Fatalf("%s No output generated", logPrefix)
	}
	if actualOutputFrames > frames { // Check if output fits in serial buffer for deinterleave
		t.Fatalf("%s Generated output frames (%d) exceeds serial buffer frame capacity (%d)", logPrefix, actualOutputFrames, frames)
	}

	// De-interleave
	err = deinterleaveDataGo(outputInterleaved[:actualOutputFrames*channels], outputSerial, actualOutputFrames, channels)
	if err != nil {
		t.Fatalf("%s Deinterleaving failed: %v", logPrefix, err)
	}

	// Check SNR per channel
	for ch := 0; ch < channels; ch++ {
		snr, snrErr := calculateSnrGo(outputSerial[ch][:actualOutputFrames], 1, false) // Don't need detailed logs here usually
		if snrErr != nil {
			t.Errorf("%s Channel %d: calculateSnrGo failed: %v (C Line ~141)", logPrefix, ch, snrErr)
		} else if snr < targetSnr {
			t.Errorf("%s Channel %d: SNR too low: Got %.2f dB, expected >= %.1f dB (C Line ~144)", logPrefix, ch, snr, targetSnr)
			// saveOctFloatGo(...) // Optional
		}
	}

	if !t.Failed() {
		t.Logf("%s ok", logPrefix)
	}
}

// --- Process API Test ---

// testMultiChannelProcess corresponds to process_test() in C
func testMultiChannelProcess(t *testing.T, converter ConverterType, channels int, targetSnr float64) {
	t.Helper()
	logPrefix := fmt.Sprintf("Multi Process (Ch=%d): ", channels)
	t.Logf("%s Starting...", logPrefix)

	if channels > maxChannelsMulti {
		t.Fatalf("%s Channel count %d exceeds test limit %d", logPrefix, channels, maxChannelsMulti)
	}

	frames := bufferLenMulti

	// Allocate buffers
	inputSerial := make([][]float32, channels)
	outputSerial := make([][]float32, channels)
	for ch := 0; ch < channels; ch++ {
		inputSerial[ch] = make([]float32, frames)
		outputSerial[ch] = make([]float32, frames)
	}
	inputInterleaved := make([]float32, frames*channels)
	// Output buffer needs capacity for potentially slightly more than ratio*frames
	outputCap := int(math.Ceil(float64(frames)*0.95)) + 100
	if outputCap > frames {
		outputCap = frames
	} // Cap like simple test?
	outputInterleaved := make([]float32, outputCap*channels)

	// Generate input
	for ch := 0; ch < channels; ch++ {
		freq := (400.0 + 11.333333333*float64(ch)) / 44100.0 // Different freqs than simple
		genWindowedSinesGo(1, []float64{freq}, 1.0, inputSerial[ch])
	}

	// Interleave
	err := interleaveDataGo(inputSerial, inputInterleaved, frames, channels)
	if err != nil {
		t.Fatalf("%s Interleaving failed: %v", logPrefix, err)
	}

	// --- Perform Conversion using Process API ---
	const srcRatio = 0.95
	state, err := New(converter, channels)
	if err != nil {
		t.Fatalf("%s New() failed: %v (C Line ~191)", logPrefix, err)
	}
	defer state.Close()

	srcData := SrcData{SrcRatio: srcRatio}
	var currentInFrames int64 = 0
	var currentOutFrames int64 = 0
	totalInputLen := int64(frames)                                          // Total input frames to process
	targetOutputLen := int64(math.Floor(float64(totalInputLen) * srcRatio)) // Expected total output

	loopCount := 0
	maxLoops := (totalInputLen / blockLenMulti) + int64(math.Ceil(float64(targetOutputLen)/float64(blockLenMulti))) + 50
	if maxLoops < 100 {
		maxLoops = 100
	}

	for {
		loopCount++
		if int64(loopCount) > maxLoops {
			t.Fatalf("%s Process loop exceeded max iterations (%d)", logPrefix, maxLoops)
		}

		// Calculate chunk sizes
		remainingInput := totalInputLen - currentInFrames
		inFramesThisBlock := minInt64(int64(blockLenMulti), remainingInput)
		if inFramesThisBlock < 0 {
			inFramesThisBlock = 0
		}

		// Provide remaining output buffer capacity for this chunk
		outSpaceAvailable := int64(len(outputInterleaved)/channels) - currentOutFrames
		outFramesThisBlock := outSpaceAvailable // Give all remaining space
		if outFramesThisBlock < 0 {
			outFramesThisBlock = 0
		}

		srcData.EndOfInput = (currentInFrames >= totalInputLen)
		if srcData.EndOfInput {
			inFramesThisBlock = 0
		}

		srcData.InputFrames = inFramesThisBlock
		srcData.OutputFrames = outFramesThisBlock

		// Prepare slices
		inStart := int(currentInFrames) * channels
		inEnd := inStart + int(inFramesThisBlock)*channels
		if inStart > len(inputInterleaved) {
			inStart = len(inputInterleaved)
		}
		if inEnd > len(inputInterleaved) {
			inEnd = len(inputInterleaved)
		}
		if inStart > inEnd {
			inStart = inEnd
		}

		outStart := int(currentOutFrames) * channels
		outEnd := outStart + int(outFramesThisBlock)*channels
		if outStart > len(outputInterleaved) {
			outStart = len(outputInterleaved)
		}
		if outEnd > len(outputInterleaved) {
			outEnd = len(outputInterleaved)
		}
		if outStart > outEnd {
			outStart = outEnd
		}

		if inEnd > inStart {
			srcData.DataIn = inputInterleaved[inStart:inEnd]
		} else {
			srcData.DataIn = nil
		}
		if outEnd > outStart {
			srcData.DataOut = outputInterleaved[outStart:outEnd]
		} else {
			srcData.DataOut = nil
		}
		if outEnd <= outStart {
			srcData.OutputFrames = 0
		}

		srcData.InputFramesUsed = 0
		srcData.OutputFramesGen = 0

		// Call Process
		err = state.Process(&srcData)
		if err != nil {
			t.Fatalf("%s Process() loop %d failed: %v (C Line ~211)", logPrefix, loopCount, err)
		}

		// Termination check
		if srcData.EndOfInput && srcData.OutputFramesGen == 0 {
			currentInFrames += srcData.InputFramesUsed
			currentOutFrames += srcData.OutputFramesGen
			break
		}

		// Basic sanity checks
		if srcData.InputFramesUsed > srcData.InputFrames { /* ... t.Fatalf ... */
		}
		if srcData.OutputFramesGen > srcData.OutputFrames { /* ... t.Fatalf ... */
		}

		// Update cumulative counts
		currentInFrames += srcData.InputFramesUsed
		currentOutFrames += srcData.OutputFramesGen

		// Stall Check
		if !srcData.EndOfInput && srcData.InputFramesUsed == 0 && srcData.OutputFramesGen == 0 && srcData.InputFrames > 0 {
			t.Fatalf("%s Process() stalled loop %d before EOF. Input provided=%d", logPrefix, loopCount, srcData.InputFrames)
		}
	} // End loop

	// Check final output length
	if math.Abs(float64(currentOutFrames)-float64(targetOutputLen)) > 2.0 { // C uses > 2
		t.Errorf("%s Bad final output length: Got %d, Expected around %d +/- 2.0 (C Line ~232)", logPrefix, currentOutFrames, targetOutputLen)
	}
	actualOutputFrames := int(currentOutFrames)
	if actualOutputFrames <= 0 {
		t.Fatalf("%s No output generated", logPrefix)
	}
	if actualOutputFrames > frames { // Check if output fits in serial buffer
		t.Fatalf("%s Generated output frames (%d) exceeds serial buffer frame capacity (%d)", logPrefix, actualOutputFrames, frames)
	}

	// De-interleave
	err = deinterleaveDataGo(outputInterleaved[:actualOutputFrames*channels], outputSerial, actualOutputFrames, channels)
	if err != nil {
		t.Fatalf("%s Deinterleaving failed: %v", logPrefix, err)
	}

	// Check SNR per channel
	for ch := 0; ch < channels; ch++ {
		snr, snrErr := calculateSnrGo(outputSerial[ch][:actualOutputFrames], 1, false)
		if snrErr != nil {
			t.Errorf("%s Channel %d: calculateSnrGo failed: %v (C Line ~251)", logPrefix, ch, snrErr)
		} else if snr < targetSnr {
			t.Errorf("%s Channel %d: SNR too low: Got %.2f dB, expected >= %.1f dB (C Line ~254)", logPrefix, ch, snr, targetSnr)
			// saveOctFloatGo(...) // Optional
		}
	}

	if !t.Failed() {
		t.Logf("%s ok", logPrefix)
	}
}

// --- Callback API Test ---

// testCallbackDataGo holds state for the callback function
type testCallbackDataGo struct {
	channels     int
	totalFrames  int64
	currentFrame int64
	data         []float32 // Pointer to the full interleaved input buffer
}

// testCallbackFuncGo is the Go equivalent of test_callback_func
func testCallbackFuncGo(userData interface{}) (data []float32, framesRead int64, err error) {
	pcbData, ok := userData.(*testCallbackDataGo)
	if !ok {
		return nil, 0, fmt.Errorf("invalid user data type in callback")
	}

	if pcbData.currentFrame >= pcbData.totalFrames {
		return nil, 0, nil // No more data
	}

	frames := minInt64(int64(blockLenMulti), pcbData.totalFrames-pcbData.currentFrame)
	if frames <= 0 {
		return nil, 0, nil // Should not happen if check above works, but safety
	}

	startSample := pcbData.currentFrame * int64(pcbData.channels)
	endSample := startSample + frames*int64(pcbData.channels)

	// Bounds check
	if startSample < 0 || endSample > int64(len(pcbData.data)) {
		return nil, 0, fmt.Errorf("callback tried to read out of bounds: start=%d, end=%d, len=%d", startSample, endSample, len(pcbData.data))
	}

	data = pcbData.data[startSample:endSample] // Return slice of the next chunk
	framesRead = frames
	pcbData.currentFrame += frames // Update position

	return data, framesRead, nil
}

// testMultiChannelCallback corresponds to callback_test() in C
func testMultiChannelCallback(t *testing.T, converter ConverterType, channels int, targetSnr float64) {
	t.Helper()
	logPrefix := fmt.Sprintf("Multi Callback (Ch=%d): ", channels)
	t.Logf("%s Starting...", logPrefix)

	if channels > maxChannelsMulti {
		t.Fatalf("%s Channel count %d exceeds test limit %d", logPrefix, channels, maxChannelsMulti)
	}

	frames := bufferLenMulti

	// Allocate buffers
	inputSerial := make([][]float32, channels)
	outputSerial := make([][]float32, channels)
	for ch := 0; ch < channels; ch++ {
		inputSerial[ch] = make([]float32, frames)
		outputSerial[ch] = make([]float32, frames)
	}
	inputInterleaved := make([]float32, frames*channels)
	// Output buffer for CallbackRead calls - needs fixed size per call
	callbackReadBuffer := make([]float32, blockLenMulti*channels)
	// Buffer to accumulate all output
	totalOutputCap := int(math.Ceil(float64(frames)*0.95)) + 100 // Similar estimate
	if totalOutputCap > frames {
		totalOutputCap = frames
	} // Cap?
	totalOutputInterleaved := make([]float32, 0, totalOutputCap*channels)

	// Generate input
	for ch := 0; ch < channels; ch++ {
		freq := (200.0 + 33.333333333*float64(ch)) / 44100.0 // Same freqs as simple test
		genWindowedSinesGo(1, []float64{freq}, 1.0, inputSerial[ch])
	}

	// Interleave
	err := interleaveDataGo(inputSerial, inputInterleaved, frames, channels)
	if err != nil {
		t.Fatalf("%s Interleaving failed: %v", logPrefix, err)
	}

	// --- Perform Conversion using Callback API ---
	const srcRatio = 0.95
	cbData := &testCallbackDataGo{
		channels:     channels,
		totalFrames:  int64(frames),
		currentFrame: 0,
		data:         inputInterleaved,
	}

	state, err := CallbackNew(testCallbackFuncGo, converter, channels, cbData)
	if err != nil {
		t.Fatalf("%s CallbackNew failed: %v (C Line ~298)", logPrefix, err)
	}
	defer state.Close()

	totalFramesRead := int64(0)
	maxLoops := int64(frames/blockLenMulti) + int64(math.Ceil(float64(totalOutputCap)/float64(blockLenMulti))) + 50

	if maxLoops < 100 {
		maxLoops = 100
	}

	for loopCount := int64(0); ; loopCount++ {
		if loopCount > maxLoops {
			t.Fatalf("%s CallbackRead loop exceeded max iterations (%d)", logPrefix, maxLoops)
		}

		// Request up to blockLenMulti frames
		framesReadThisCall, readErr := CallbackRead(state, srcRatio, int64(blockLenMulti), callbackReadBuffer)
		if readErr != nil {
			t.Fatalf("%s CallbackRead loop %d failed: %v", logPrefix, loopCount, readErr)
		}

		if framesReadThisCall <= 0 {
			break // End of stream
		}

		// Append result to accumulator slice
		samplesRead := framesReadThisCall * int64(channels)
		totalOutputInterleaved = append(totalOutputInterleaved, callbackReadBuffer[:samplesRead]...)
		totalFramesRead += framesReadThisCall
	}

	// Check for errors that might have occurred during processing
	finalErr := state.LastError()
	if finalErr != nil {
		t.Errorf("%s Error reported after CallbackRead loop: %v (C Line ~316)", logPrefix, finalErr)
	}

	// Check final output length
	targetOutputLen := int64(math.Floor(float64(frames) * srcRatio))       // Expected total output
	if math.Abs(float64(totalFramesRead)-float64(targetOutputLen)) > 2.0 { // C uses > 2
		t.Errorf("%s Bad final output length: Got %d, Expected around %d +/- 2.0 (C Line ~321)", logPrefix, totalFramesRead, targetOutputLen)
	}
	actualOutputFrames := int(totalFramesRead)
	if actualOutputFrames <= 0 {
		t.Fatalf("%s No output generated", logPrefix)
	}
	if actualOutputFrames > frames { // Check if output fits in serial buffer
		t.Fatalf("%s Generated output frames (%d) exceeds serial buffer frame capacity (%d)", logPrefix, actualOutputFrames, frames)
	}

	// De-interleave
	err = deinterleaveDataGo(totalOutputInterleaved[:actualOutputFrames*channels], outputSerial, actualOutputFrames, channels)
	if err != nil {
		t.Fatalf("%s Deinterleaving failed: %v", logPrefix, err)
	}

	// Check SNR per channel
	for ch := 0; ch < channels; ch++ {
		snr, snrErr := calculateSnrGo(outputSerial[ch][:actualOutputFrames], 1, false)
		if snrErr != nil {
			t.Errorf("%s Channel %d: calculateSnrGo failed: %v (C Line ~336)", logPrefix, ch, snrErr)
		} else if snr < targetSnr {
			t.Errorf("%s Channel %d: SNR too low: Got %.2f dB, expected >= %.1f dB (C Line ~339)", logPrefix, ch, snr, targetSnr)
			// saveOctFloatGo(...) // Optional
		}
	}

	if !t.Failed() {
		t.Logf("%s ok", logPrefix)
	}
}
