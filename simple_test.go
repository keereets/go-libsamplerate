// samplerate_simple_test.go
package libsamplerate

import (
	"fmt"
	"math"
	"testing"
)

const (
	// bufferLen corresponds to BUFFER_LEN in simple_test.c
	bufferLenSimpleTest = 2048
	// numFramesProducesTest corresponds to NUM_FRAMES in src_simple_produces_output
	numFramesProducesTest = 1000
)

// TestSimpleAPI corresponds to the main function driving the tests in simple_test.c
func TestSimpleAPI(t *testing.T) {
	srcRatios := []float64{
		1.0001, 0.099, 0.1, 0.33333333, 0.789, 1.9, 3.1, 9.9, 256.0, 1.0 / 256.0,
	}

	tests := []struct {
		name      string
		converter ConverterType
		enabled   bool // To mimic #ifdef / check internal const
	}{
		// Check if your libsamplerate Go package exposes constants indicating
		// which converters are enabled. Adjust the 'enabled' field accordingly.
		// Assuming they are enabled if the type exists for now.
		{"ZeroOrderHold", ZeroOrderHold, true},
		{"Linear", Linear, true},
		// If you have a constant like 'EnableSincFastConverter'
		// {"SincFastest", SincFastest, EnableSincFastConverter},
		// Otherwise, assume true if the type exists:
		{"SincFastest", SincFastest, true}, // Adjust if needed
	}

	for _, tt := range tests {
		converterTest := tt // Capture range variable for subtests
		if !converterTest.enabled {
			t.Logf("Skipping %s tests (disabled in build or Go package)", converterTest.name)
			continue
		}

		// Run tests for this converter type
		t.Run(converterTest.name, func(t *testing.T) {
			for _, ratio := range srcRatios {
				currentRatio := ratio // Capture range variable

				// Subtest corresponding to simple_test() C function
				t.Run(fmt.Sprintf("Ratio_%.4f_CheckOutputLength", currentRatio), func(t *testing.T) {
					// Run in parallel if tests are independent and thread-safe
					// t.Parallel()
					testSimpleOutputLength(t, converterTest.converter, currentRatio)
				})

				// Subtest corresponding to src_simple_produces_output_test() / src_simple_produces_output() C functions
				t.Run(fmt.Sprintf("Ratio_%.4f_ProducesOutput", currentRatio), func(t *testing.T) {
					// Loop through channels 1 to 9
					for channels := 1; channels <= 9; channels++ {
						currentChannels := channels // Capture range variable
						t.Run(fmt.Sprintf("Channels_%d", currentChannels), func(t *testing.T) {
							// Run in parallel if tests are independent and thread-safe
							// t.Parallel()
							testSimpleProducesOutput(t, converterTest.converter, currentChannels, currentRatio)
						})
					}
				})
			}
		})
	}
}

// testSimpleProducesOutput corresponds to src_simple_produces_output in simple_test.c
// It verifies that src_simple runs without error and processes/generates some frames.
func testSimpleProducesOutput(t *testing.T, converterType ConverterType, channels int, srcRatio float64) {
	input := make([]float32, numFramesProducesTest*channels)
	// Output buffer needs sufficient capacity. C used the same size, which might
	// truncate output for ratios > 1, but the C test only checks if *any* output is generated.
	// Let's allocate enough capacity assuming output might be larger.
	// Max possible output size for src_ratio is roughly input_frames * src_ratio.
	// Add some buffer.
	outputCap := int(math.Ceil(float64(numFramesProducesTest)*srcRatio)) + 100 // Generous capacity
	output := make([]float32, outputCap*channels)

	// Input data is zeroed by make(), matching calloc.

	srcData := SrcData{
		DataIn:       input,
		InputFrames:  numFramesProducesTest,
		DataOut:      output,
		OutputFrames: int64(outputCap), // Provide full capacity
		SrcRatio:     srcRatio,
		EndOfInput:   true, // src_simple implies a single block
	}

	// Mimic C: printf ("\tproduces_output\t(SRC ratio = %6.4f, channels = %d) ... ", src_ratio, channels) ; fflush (stdout) ;
	t.Logf("Testing produces_output (Ratio=%.4f, Chan=%d)", srcRatio, channels)

	err := Simple(&srcData, converterType, channels)
	if err != nil {
		// Mimic C: printf ("\n\nLine %d : %s\n\n", __LINE__, src_strerror (error)) ; exit (1) ;
		t.Fatalf("Simple failed: %v (C Line ~98)", err)
	}

	if srcData.InputFramesUsed == 0 {
		// Mimic C: printf ("\n\nLine %d : No input frames used.\n\n", __LINE__) ; exit (1) ;
		t.Fatalf("No input frames used (Ratio=%.4f, Chan=%d) (C Line ~102)", srcRatio, channels)
	}

	if srcData.OutputFramesGen == 0 {
		// Mimic C: printf ("\n\nLine %d : No output frames generated.\n\n", __LINE__) ; exit (1) ;
		t.Fatalf("No output frames generated (Ratio=%.4f, Chan=%d) (C Line ~106)", srcRatio, channels)
	}

	// Mimic C: puts ("ok") ;
	// If we reach here without Fatalf, the subtest passes implicitly.
	// t.Logf("ok") // Optional: explicit success log
}

// testSimpleOutputLength corresponds to simple_test in simple_test.c
// It verifies that the number of output frames generated is close to the expected value.
func testSimpleOutputLength(t *testing.T, converterType ConverterType, srcRatio float64) {
	// Mimic C: printf ("\tsimple_test\t(SRC ratio = %6.4f) ................. ", src_ratio) ; fflush (stdout) ;
	t.Logf("Testing simple_test output length check (Ratio=%.4f)", srcRatio)

	// Calculate input and output lengths based on C logic
	var inputLenCalc int
	// C code's output_len seems just for a sanity check, not buffer allocation size.
	// C uses static buffers of BUFFER_LEN. Go needs explicit allocation.
	outputBufferCap := bufferLenSimpleTest // Capacity of the Go output buffer

	if srcRatio >= 1.0 {
		// calc output_len = BUFFER_LEN ; // Not used directly for buffer size
		inputLenCalc = int(math.Floor(float64(bufferLenSimpleTest) / srcRatio))
	} else {
		inputLenCalc = bufferLenSimpleTest
		// calc output_len = floor (BUFFER_LEN * src_ratio) ; // Check value
		calcOutputLenCheck := int(math.Floor(float64(bufferLenSimpleTest) * srcRatio))
		// Sanity check from C code
		if calcOutputLenCheck > bufferLenSimpleTest {
			t.Fatalf("Internal calculation error: calcOutputLenCheck (%d) > bufferLen (%d) (C Line ~152)", calcOutputLenCheck, bufferLenSimpleTest)
		}
	}

	// Crucial adjustment from C test: Reduce input_len by 10
	inputLenCalc -= 10
	if inputLenCalc <= 0 {
		t.Skipf("Calculated input length <= 0 (%d), skipping test for ratio %.4f", inputLenCalc, srcRatio)
		return
	}

	// Allocate Go buffers
	inputData := make([]float32, inputLenCalc)     // Use calculated length
	outputData := make([]float32, outputBufferCap) // Use full capacity

	// Input data is zeroed by make()

	srcData := SrcData{
		DataIn:       inputData,
		InputFrames:  int64(len(inputData)), // Use actual length
		DataOut:      outputData,
		OutputFrames: int64(len(outputData)), // Provide full capacity
		SrcRatio:     srcRatio,
		EndOfInput:   true, // src_simple implies a single block
	}

	// Call the Go equivalent of src_simple
	err := Simple(&srcData, converterType, 1) // C test uses 1 channel here
	if err != nil {
		// Mimic C: printf ("\n\nLine %d : %s\n\n", __LINE__, src_strerror (error)) ; exit (1) ;
		t.Fatalf("Simple failed: %v (C Line ~168)", err)
	}

	// Check the output frames generated count, mimicking C's check
	// terminate = (int) ceil ((src_ratio >= 1.0) ? src_ratio : 1.0 / src_ratio) ;
	terminateF := math.Ceil(math.Max(srcRatio, 1.0/srcRatio))
	// tolerance = 2 * terminate
	tolerance := 2.0 * terminateF

	// expected frames = src_ratio * input_len (original calculated input len before providing to func)
	expectedFramesF := srcRatio * float64(inputLenCalc)

	// diff = fabs (src_data.output_frames_gen - expected frames)
	diff := math.Abs(float64(srcData.OutputFramesGen) - expectedFramesF)

	if diff > tolerance {
		// Mimic C: printf ("\n\nLine %d : bad output data length %ld should be %d.\n", ...) ; exit (1) ;
		t.Errorf("Bad output data length: Got %d, expected approx %.2f (tolerance %.2f) (C Line ~171)",
			srcData.OutputFramesGen, expectedFramesF, tolerance)
		t.Logf("Details: Ratio=%.4f, InputFed=%d, InputUsed=%d, TerminateFactor=%.1f",
			srcRatio, inputLenCalc, srcData.InputFramesUsed, terminateF)
	} else {
		// Mimic C: puts ("ok") ;
		// Success is implicit if no Errorf/Fatalf
		// t.Logf("ok") // Optional
	}
}

// You might need a minInt helper if not already present
// func minInt(a, b int) int {
// 	if a < b {
// 		return a
// 	}
// 	return b
// }
