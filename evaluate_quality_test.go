//go:build fftw_required

//Needed for calculateSnrGo

// inspired by src-evaluate.c

package libsamplerate

import (
	"fmt"
	"math"
	"testing"
	// Assumes types, constants, New, Process, etc. are defined
	// Assumes helpers genWindowedSinesGo, calculateSnrGo are in test_utils.go
)

const (
	evaluateBufferLen = 80000   // From C BUFFER_LEN
	evaluateInputRate = 44100.0 // Fixed input rate from C generate_source_wav
)

// Corresponds to SNR_TEST struct in C, but only holds test parameters
type evaluationSnrTestParams struct {
	testDesc         string // Added for clarity
	freqCount        int
	freqs            [maxFreqs]float64
	outputSampleRate int
	passBandPeaks    int // Expected peaks for calculateSnrGo
	// expectedPeak    float64 // C test doesn't check peak here
}

// Test data parameters from C measure_snr function
var evaluationSnrTestData = []evaluationSnrTestParams{
	{"SR 48k Single Low Freq", 1, [maxFreqs]float64{0.211111111111}, 48000, 1},
	{"SR 132k Single Low Freq", 1, [maxFreqs]float64{0.011111111111}, 132301, 1},  // Unusual rate
	{"SR 92k Single Mid Freq", 1, [maxFreqs]float64{0.111111111111}, 92301, 1},    // Unusual rate
	{"SR 26k Single Low Freq", 1, [maxFreqs]float64{0.011111111111}, 26461, 1},    // Unusual rate
	{"SR 13k Single Low Freq", 1, [maxFreqs]float64{0.011111111111}, 13231, 1},    // Unusual rate
	{"SR 44.1k Near Unity", 1, [maxFreqs]float64{0.011111111111}, 44101, 1},       // Near unity ratio
	{"SR 78k Dual Tone High", 2, [maxFreqs]float64{0.311111, 0.49}, 78199, 2},     // Unusual rate, high freqs
	{"SR 12k Dual Tone Low/High", 2, [maxFreqs]float64{0.011111, 0.49}, 12345, 1}, // Downsample, expect only low peak
	{"SR 20k Dual Tone Low/Mid", 2, [maxFreqs]float64{0.0123456, 0.4}, 20143, 1},  // Downsample, expect only low peak
	{"SR 26k Dual Tone Low/Mid", 2, [maxFreqs]float64{0.0111111, 0.4}, 26461, 1},  // Downsample, expect only low peak
	{"SR 58k Single High Freq", 1, [maxFreqs]float64{0.381111111111}, 58661, 1},   // Unusual rate
}

// TestEvaluateQualityAPI runs SNR measurements for the Go libsamplerate port
// using test conditions derived from src-evaluate.c's measure_snr function.
// It logs the measured SNR but doesn't compare against external program results.
func TestEvaluateQualityAPI(t *testing.T) {
	convertersToTest := []struct {
		name      string
		converter ConverterType
		enabled   bool
	}{
		{"ZeroOrderHold", ZeroOrderHold, true},
		{"Linear", Linear, true},
		{"SincFastest", SincFastest, enableSincFastConverter},
		{"SincMedium", SincMediumQuality, enableSincMediumConverter},
		{"SincBest", SincBestQuality, enableSincBestConverter},
	}

	inputBuffer := make([]float32, evaluateBufferLen) // Reusable input buffer

	t.Logf("\n--- Go libsamplerate Quality Evaluation (based on src-evaluate conditions) ---")

	for _, ct := range convertersToTest {
		converterTest := ct // Capture
		if !converterTest.enabled {
			t.Logf("\nSkipping Converter: %s (disabled)", converterTest.name)
			continue
		}

		t.Run(converterTest.name, func(t *testing.T) {
			t.Logf("\n--- Testing Converter: %s ---", converterTest.name)

			for i, params := range evaluationSnrTestData {
				testParams := params // Capture
				testName := fmt.Sprintf("Test_%d_%s_OutSR_%d", i, testParams.testDesc, testParams.outputSampleRate)

				t.Run(testName, func(t *testing.T) {
					// Generate input signal (fixed 44.1kHz rate)
					genWindowedSinesGo(testParams.freqCount, testParams.freqs[:testParams.freqCount], 0.9, inputBuffer) // Amp 0.9 matches C

					// Calculate ratio and estimate output size
					srcRatio := float64(testParams.outputSampleRate) / evaluateInputRate
					if isBadSrcRatio(srcRatio) { // Use helper from package
						t.Skipf("Skipping test - Bad SRC Ratio %.5f (OutSR=%d)", srcRatio, testParams.outputSampleRate)
						return
					}
					outputFramesEstimate := int64(math.Ceil(float64(len(inputBuffer))*srcRatio)) + 100 // Estimate + margin
					outputBuffer := make([]float32, outputFramesEstimate)                              // Allocate output buffer for this test run

					// --- Perform Conversion using Go libsamplerate ---
					state, err := New(converterTest.converter, 1) // Mono
					if err != nil {
						t.Fatalf("New() failed: %v", err)
					}
					// No need to defer Close if using Simple, but Process needs it
					// C code used external process, let's mimic one-shot with Simple?
					// No, C code seems to imply using src_process for timing? Let's use Process.
					defer state.Close()

					srcData := SrcData{
						DataIn:       inputBuffer,
						InputFrames:  int64(len(inputBuffer)),
						DataOut:      outputBuffer,
						OutputFrames: int64(len(outputBuffer)),
						SrcRatio:     srcRatio,
						EndOfInput:   true, // Treat as one block, like processing a file
					}

					err = state.Process(&srcData)
					if err != nil {
						t.Fatalf("Process() failed: %v", err)
					}

					actualOutputLen := int(srcData.OutputFramesGen)
					if actualOutputLen <= 0 {
						t.Fatalf("Process() generated no output")
					}
					if actualOutputLen > len(outputBuffer) {
						t.Fatalf("Process() generated more output (%d) than buffer capacity (%d)", actualOutputLen, len(outputBuffer))
					}

					// --- Measure SNR ---
					snr, snrErr := calculateSnrGo(outputBuffer[:actualOutputLen], testParams.passBandPeaks)

					logPrefix := fmt.Sprintf("Ratio %.4f (SR %d->%d): ", srcRatio, int(evaluateInputRate), testParams.outputSampleRate)
					if snrErr != nil {
						t.Errorf("%s SNR Calculation Failed: %v", logPrefix, snrErr)
					} else {
						// Log the result - C code didn't compare to threshold here
						t.Logf("%s Measured SNR = %.2f dB", logPrefix, snr)
						// Optionally add checks here if you establish expected baselines for the Go port
						// e.g., if snr < some_minimum_quality_threshold { t.Errorf(...) }
					}

					// Verify input consumed (Process with EOF should consume all)
					if srcData.InputFramesUsed != int64(len(inputBuffer)) {
						t.Errorf("%s Did not consume all input: Used %d, Expected %d", logPrefix, srcData.InputFramesUsed, len(inputBuffer))
					}

				}) // End t.Run for specific SNR test
			} // End loop over test params
		}) // End t.Run for converter type
	} // End loop over converters
}
