// downsample_test.go
package libsamplerate // Test file is in the same package

import (
	"fmt"
	"math"
	"testing"
)

// TestDownsampleSimple corresponds to downsample_test.c
func TestDownsampleSimple(t *testing.T) {
	// Define input/output buffer sizes
	const inLen = 1000
	const outLen = 10

	// Prepare input data (optional, C code didn't initialize, but useful)
	// Let's create a simple ramp
	in := make([]float32, inLen)
	for i := range in {
		in[i] = float32(i) / float32(inLen) // Simple ramp 0.0 to ~1.0
	}

	// Output buffer
	out := make([]float32, outLen)

	// Define the test cases for each converter type
	testCases := []struct {
		converterType ConverterType
		enabled       bool // Use config constants
	}{
		{ZeroOrderHold, true}, // Always enabled
		{Linear, true},        // Always enabled
		{SincFastest, enableSincFastConverter},
		{SincMediumQuality, enableSincMediumConverter},
		{SincBestQuality, enableSincBestConverter},
	}

	// Ratio for downsampling
	ratio := 1.0 / 255.0

	t.Log("Running Downsample Simple tests...")

	for _, tc := range testCases {
		// Skip test if the converter wasn't enabled at "build time" (via config consts)
		if !tc.enabled {
			t.Logf("        Skipping test for %-28s (disabled)", GetName(tc.converterType))
			continue
		}

		// Use t.Run to create a subtest for each converter type
		// This gives clearer output if one fails
		converterName := GetName(tc.converterType)
		if converterName == "" {
			converterName = fmt.Sprintf("Unknown Converter (%d)", tc.converterType)
		}

		t.Run(converterName, func(t *testing.T) {
			t.Logf("        Testing %-28s ....... ", converterName)

			// Prepare SRC_DATA struct for this run
			// Important: Reset output buffer if reusing across subtests,
			// though Simple should fill it. Let's zero it for safety.
			for i := range out {
				out[i] = 0.0
			}

			data := SrcData{
				DataIn:       in,
				DataOut:      out,
				InputFrames:  int64(len(in)),  // Use actual length
				OutputFrames: int64(len(out)), // Use actual length
				SrcRatio:     ratio,
				EndOfInput:   true, // src_simple implies end_of_input = true
				// InputFramesUsed and OutputFramesGen will be set by Simple/Process
			}

			// Call the Simple function (Go equivalent of src_simple)
			err := Simple(&data, tc.converterType, 1) // Assuming 1 channel based on C test

			// Check for errors
			if err != nil {
				t.Errorf("FAILED: Simple() with %s returned error: %v", converterName, err)
				// Optionally log data counts if needed for debugging
				// t.Logf("InputFramesUsed: %d, OutputFramesGen: %d", data.InputFramesUsed, data.OutputFramesGen)
			} else {
				t.Logf("ok (InUsed: %d, OutGen: %d)", data.InputFramesUsed, data.OutputFramesGen)
				// Basic sanity check on output generation (optional)
				if data.OutputFramesGen == 0 && len(in) > 0 {
					// With this ratio, we expect *some* output
					t.Logf("WARNING: Generated 0 output frames from %d input frames.", len(in))
				}
				if data.OutputFramesGen > int64(len(out)) {
					t.Errorf("FAIL: Generated %d frames, exceeding output buffer size %d", data.OutputFramesGen, len(out))
				}
			}
		})
	}

	t.Log("Downsample Simple tests finished.")
}

// --- Helper function to generate sine wave (Optional, instead of ramp) ---
func generateSine(buffer []float32, freq float64, sampleRate float64) {
	for i := range buffer {
		buffer[i] = float32(math.Sin(2.0 * math.Pi * freq * float64(i) / sampleRate))
	}
}
