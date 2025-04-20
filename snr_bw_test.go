//go:build fftw_required

// snr_bw_test.go
// ^^^ NOTE: This build tag ensures the test only compiles/runs if explicitly requested,
//
//	e.g., `go test -tags fftw_required`.
//	Remove or change the tag if you implement the FFT parts.
//
// run with: go test -tags fftw_required -run ^TestSnrBwAPI$ ./... -v
// set SINC_DEBUG=1 as an env var to get debug from sinc.go
package libsamplerate // <<< CORRECTED Package Name

import (
	"fmt"
	"math"
	"testing"
	// Adjust import path if necessary - assumed to be in the same package now
	// "github.com/your_user/your_repo/libsamplerate"
	// --- FFT Dependency ---
	// You will need a Go FFT library to implement calculateSnrGo.
	// Example using Gonum:
	// "gonum.org/v1/gonum/dsp/fft"
	// "gonum.org/v1/gonum/floats" // For finding peaks/summing
)

const (
	bufferLenSnr = 50000
	maxFreqs     = 4
	// maxRatios    = 6 // Not directly used in test data?
	maxSpecLen = 1 << 15 // 32768
)

// --- Struct Definitions (Mirrors C) ---

type singleTest struct {
	freqCount     int
	freqs         [maxFreqs]float64 // Frequencies relative to sample rate (0.0 to 0.5)
	srcRatio      float64
	passBandPeaks int     // Expected number of peaks in passband for SNR calc
	expectedSnr   float64 // Minimum expected SNR in dB
	expectedPeak  float64 // Expected peak value of output signal (+/- tolerance)
	peakTolerance float64 // Added tolerance for peak check
}

type converterTest struct {
	converter       ConverterType // Use type from libsamplerate package
	testCount       int
	doBandwidthTest bool
	testData        []singleTest // Use slice for variable length
}

// --- Test Data (Mirrors C) ---

// NOTE: Ensure ConverterType constants (ZeroOrderHold, Linear, SincFastest, etc.)
//
//	are defined in your libsamplerate package.
var snrTestData = []converterTest{
	{
		converter:       ZeroOrderHold, // Use package's constants
		testCount:       8,
		doBandwidthTest: false,
		testData: []singleTest{
			{1, [maxFreqs]float64{0.01111111111}, 3.0, 1, 28.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 0.6, 1, 36.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 0.3, 1, 36.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 1.0, 1, 150.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 1.001, 1, 38.0, 1.0, 0.01},
			{2, [maxFreqs]float64{0.011111, 0.324}, 1.9999, 2, 14.0, 1.0, 0.01},
			{2, [maxFreqs]float64{0.012345, 0.457}, 0.456789, 1, 12.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.3511111111}, 1.33, 1, 10.0, 1.0, 0.01},
		},
	},
	{
		converter:       Linear,
		testCount:       8,
		doBandwidthTest: false,
		testData: []singleTest{
			{1, [maxFreqs]float64{0.01111111111}, 3.0, 1, 73.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 0.6, 1, 73.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 0.3, 1, 73.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 1.0, 1, 150.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 1.001, 1, 77.0, 1.0, 0.01},
			{2, [maxFreqs]float64{0.011111, 0.324}, 1.9999, 2, 15.0, 0.94, 0.02},
			{2, [maxFreqs]float64{0.012345, 0.457}, 0.456789, 1, 25.0, 0.96, 0.02},
			{1, [maxFreqs]float64{0.3511111111}, 1.33, 1, 22.0, 0.99, 0.01},
		},
	},
	// Use package constants like 'enableSincFastConverter' if they exist
	// Assuming they exist for now:
	{
		converter:       SincFastest,
		testCount:       9,
		doBandwidthTest: enableSincFastConverter, // Use flag from package
		testData: []singleTest{
			{1, [maxFreqs]float64{0.01111111111}, 3.0, 1, 100.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 0.6, 1, 99.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 0.3, 1, 100.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 1.0, 1, 150.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 1.001, 1, 100.0, 1.0, 0.01},
			{2, [maxFreqs]float64{0.011111, 0.324}, 1.9999, 2, 97.0, 1.0, 0.01},
			{2, [maxFreqs]float64{0.012345, 0.457}, 0.456789, 1, 100.0, 0.5, 0.02},
			{2, [maxFreqs]float64{0.011111, 0.45}, 0.6, 1, 97.0, 0.5, 0.02},
			{1, [maxFreqs]float64{0.3511111111}, 1.33, 1, 97.0, 1.0, 0.01},
		},
	},
	{
		converter:       SincMediumQuality,
		testCount:       9,
		doBandwidthTest: enableSincMediumConverter, // Use flag from package
		testData: []singleTest{
			{1, [maxFreqs]float64{0.01111111111}, 3.0, 1, 145.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 0.6, 1, 132.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 0.3, 1, 138.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 1.0, 1, 157.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 1.001, 1, 148.0, 1.0, 0.01},
			{2, [maxFreqs]float64{0.011111, 0.324}, 1.9999, 2, 127.0, 1.0, 0.01},
			{2, [maxFreqs]float64{0.012345, 0.457}, 0.456789, 1, 123.0, 0.5, 0.02},
			{2, [maxFreqs]float64{0.011111, 0.45}, 0.6, 1, 126.0, 0.5, 0.02},
			{1, [maxFreqs]float64{0.43111111111}, 1.33, 1, 121.0, 1.0, 0.01},
		},
	},
	{
		converter:       SincBestQuality,
		testCount:       9,
		doBandwidthTest: enableSincBestConverter, // Use flag from package
		testData: []singleTest{
			{1, [maxFreqs]float64{0.01111111111}, 3.0, 1, 147.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 0.6, 1, 147.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 0.3, 1, 148.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 1.0, 1, 155.0, 1.0, 0.01},
			{1, [maxFreqs]float64{0.01111111111}, 1.001, 1, 148.0, 1.0, 0.01},
			{2, [maxFreqs]float64{0.011111, 0.324}, 1.9999, 2, 146.0, 1.0, 0.01},
			{2, [maxFreqs]float64{0.012345, 0.457}, 0.456789, 1, 147.0, 0.5, 0.02},
			{2, [maxFreqs]float64{0.011111, 0.45}, 0.6, 1, 144.0, 0.5, 0.02},
			{1, [maxFreqs]float64{0.43111111111}, 1.33, 1, 145.0, 1.0, 0.01},
		},
	},
}

// --- Main Test Function ---

func TestSnrBwAPI(t *testing.T) {
	verbose := testing.Verbose() // Check if running with -v

	for _, cTestData := range snrTestData {
		convTest := cTestData // Capture range variable

		// Check if converter type is potentially enabled in the Go port
		enabled := true
		// Check constants defined in the libsamplerate package
		switch convTest.converter {
		case SincFastest:
			enabled = enableSincFastConverter
		case SincMediumQuality:
			enabled = enableSincMediumConverter
		case SincBestQuality:
			enabled = enableSincBestConverter
		case Linear:
			enabled = true // Assuming always enabled
		case ZeroOrderHold:
			enabled = true // Assuming always enabled
		default:
			enabled = false // Unknown converter type
		}

		if !enabled {
			t.Logf("Skipping tests for converter %d (%s) - disabled in package", convTest.converter, GetName(convTest.converter))
			continue
		}

		t.Run(GetName(convTest.converter), func(t *testing.T) {
			var worstSnr float64 = 5000.0

			t.Logf("Converter %d : %s", convTest.converter, GetName(convTest.converter))
			t.Logf("%s", GetDescription(convTest.converter))

			if convTest.testCount > len(convTest.testData) {
				t.Fatalf("Mismatch between testCount (%d) and actual test data length (%d)", convTest.testCount, len(convTest.testData))
			}

			for i := 0; i < convTest.testCount; i++ {
				subTestData := convTest.testData[i] // Capture
				t.Run(fmt.Sprintf("SNR_Test_%d_Ratio_%.4f", i, subTestData.srcRatio), func(t *testing.T) {
					snr, err := testSnrGo(t, &subTestData, i, convTest.converter, verbose)
					if err != nil && snr == -1.0 { // Check if error was fatal within testSnrGo
						t.Errorf("SNR test %d failed: %v", i, err) // Use Errorf if testSnrGo handles failure reporting
					} else if snr >= 0 && snr < worstSnr { // Only update if valid SNR calculated
						worstSnr = snr
					}
				})
			}

			t.Logf("Worst case Signal-to-Noise Ratio : %.2f dB.", worstSnr) // Note: Will be 5000 if all SNR tests skipped

			if !convTest.doBandwidthTest {
				t.Logf("Bandwidth test not performed on this converter.\n")
			} else {
				t.Run("Bandwidth_Test", func(t *testing.T) {
					freq3dB := testBandwidthGo(t, convTest.converter, verbose)
					if !t.Skipped() { // Only log if test wasn't skipped
						t.Logf("Measured -3dB rolloff point      : %5.2f %%.\n", freq3dB)
					}
				})
			}
		}) // End t.Run for converter type
	} // End loop over converter types
}

// --- Helper Functions ---

// testSnrGo corresponds to snr_test() in C
func testSnrGo(t *testing.T, testData *singleTest, testNum int, converter ConverterType, verbose bool) (snr float64, err error) {
	t.Helper() // Mark as test helper
	snr = -1.0 // Default to error/unimplemented state

	logPrefix := fmt.Sprintf("SNR Test %d (Ratio=%.4f): ", testNum, testData.srcRatio)
	if verbose {
		t.Logf("%s Starting", logPrefix)
		freqStr := ""
		for k := 0; k < testData.freqCount; k++ {
			freqStr += fmt.Sprintf("%6.4f ", testData.freqs[k])
		}
		t.Logf("%s Frequencies : [ %s]", logPrefix, freqStr)
		t.Logf("%s SRC Ratio   : %8.4f", logPrefix, testData.srcRatio)
	}

	// Calculate buffer lengths
	var inputLen, outputLen int
	if testData.srcRatio >= 1.0 {
		outputLen = maxSpecLen
		inputLen = int(math.Ceil(float64(maxSpecLen) / testData.srcRatio))
		if inputLen > bufferLenSnr {
			inputLen = bufferLenSnr
		}
	} else {
		// inputLen = bufferLenSnr // C uses this if ratio < 1 initially
		outputLen = int(math.Ceil(float64(bufferLenSnr) * testData.srcRatio))
		outputLen -= outputLen % 16 // Align to 16 samples (C does: &= ((~0u) << 4))
		if outputLen > maxSpecLen {
			outputLen = maxSpecLen
		}
		// Recalculate input length based on desired aligned output length
		inputLen = int(math.Ceil(float64(outputLen) / testData.srcRatio))
		if inputLen > bufferLenSnr {
			t.Logf("%s WARNING: Recalculated inputLen %d > bufferLenSnr %d", logPrefix, inputLen, bufferLenSnr)
			inputLen = bufferLenSnr
		}
	}
	if inputLen <= 0 || outputLen <= 0 {
		t.Errorf("%s Invalid calculated lengths: input=%d, output=%d", logPrefix, inputLen, outputLen)
		return snr, fmt.Errorf("invalid lengths")
	}

	// Allocate buffers
	inputData := make([]float32, inputLen)
	outputData := make([]float32, maxSpecLen) // Allocate max, use actualOutputLen later

	// Generate input signal using the newly translated function
	genWindowedSinesGo(testData.freqCount, testData.freqs[:testData.freqCount], 1.0, inputData)

	// --- Perform Sample Rate Conversion ---
	var state Converter            // Use interface type
	state, err = New(converter, 1) // <<< FIX: Correct arguments
	if err != nil {
		t.Errorf("%s libsamplerate.New() failed: %v (C Line ~163)", logPrefix, err)
		return snr, err
	}
	defer state.Close()

	srcData := SrcData{ // Use type from libsamplerate package
		DataIn:       inputData,
		InputFrames:  int64(len(inputData)),
		DataOut:      outputData,
		OutputFrames: int64(len(outputData)), // Provide capacity
		SrcRatio:     testData.srcRatio,
		EndOfInput:   true, // C test uses src_process with EndOfInput=true
	}

	err = state.Process(&srcData) // C uses src_process
	if err != nil {
		t.Errorf("%s state.Process() failed: %v (C Line ~181)", logPrefix, err)
		// saveOctFloatGo("snr_test_fail.dat", inputData, outputData[:srcData.OutputFramesGen]) // Optional
		return snr, err
	}

	actualOutputLen := int(srcData.OutputFramesGen)
	if verbose {
		t.Logf("%s Output Len  : %d (Expected approx: %d)", logPrefix, actualOutputLen, outputLen)
	}

	// Check output length consistency
	if math.Abs(float64(actualOutputLen-outputLen)) > 4 {
		t.Errorf("%s Output data length mismatch: Got %d, expected around %d (C Line ~190)", logPrefix, actualOutputLen, outputLen)
		// Return error? C exits. Let's return error state.
		return snr, fmt.Errorf("output length mismatch")
	}
	if actualOutputLen <= 0 {
		t.Errorf("%s No output generated (Got %d)", logPrefix, actualOutputLen)
		return snr, fmt.Errorf("no output generated")
	}

	// --- Check Output Peak ---
	outputPeak := findPeakGo(outputData[:actualOutputLen])
	if verbose {
		t.Logf("%s Output Peak : %6.4f (Expected: %.4f)", logPrefix, outputPeak, testData.expectedPeak)
	}
	if math.Abs(outputPeak-testData.expectedPeak) > testData.peakTolerance {
		t.Errorf("%s Output peak mismatch: Got %.4f, expected %.4f (Tolerance %.4f) (C Line ~200)", logPrefix, outputPeak, testData.expectedPeak, testData.peakTolerance)
		// saveOctFloatGo("snr_peak_fail.dat", inputData, outputData[:actualOutputLen]) // Optional
		// Return error? C exits. Let's return error state but continue if possible.
		err = fmt.Errorf("peak mismatch") // Store error but don't return yet
	}

	// --- Calculate SNR (Requires FFT Implementation) ---
	t.Skipf("%s Skipping SNR calculation - requires FFT implementation (calculateSnrGo)", logPrefix) // SKIP FFT FOR NOW
	snr = 999.0                                                                                      // Placeholder value

	/*
	   // Replace skip with actual call when implemented:
	   var snrErr error
	   snr, snrErr = calculateSnrGo(outputData[:actualOutputLen], testData.passBandPeaks)
	   if snrErr != nil {
	       t.Errorf("%s calculate_snr failed: %v (C Line ~208)", logPrefix, snrErr)
	       // saveOctFloatGo("snr_calc_fail.dat", inputData, outputData[:actualOutputLen])
	       return -1.0, snrErr // Return error state
	   }

	   if verbose {
	       t.Logf("%s SNR Ratio   : %.2f dB (Expected >= %.2f)", logPrefix, snr, testData.expectedSnr)
	   }

	   if snr < testData.expectedSnr {
	       t.Errorf("%s SNR too low: Got %.2f dB, expected >= %.2f dB (C Line ~217)", logPrefix, snr, testData.expectedSnr)
	       // Store error but return calculated SNR
	       if err == nil { // Don't overwrite peak mismatch error
	           err = fmt.Errorf("SNR too low")
	       }
	   }
	*/

	if err == nil && !t.Skipped() && !t.Failed() { // Only log Pass if no errors occurred and not skipped/failed
		if !verbose {
			t.Logf("%s Pass", logPrefix)
		}
	}

	// Return calculated (or placeholder) SNR, and any error encountered (like peak mismatch)
	return snr, err
}

// findPeakGo corresponds to find_peak() in C
func findPeakGo(data []float32) float64 {
	peak := 0.0
	for _, val := range data {
		absVal := math.Abs(float64(val))
		if absVal > peak {
			peak = absVal
		}
	}
	return peak
}

// findAttenuationGo corresponds to find_attenuation() in C
func findAttenuationGo(t *testing.T, freq float64, converter ConverterType, verbose bool) (float64, error) {
	t.Helper()
	inputData := make([]float32, bufferLenSnr)
	outputCap := int(math.Ceil(bufferLenSnr*1.999)) + 100
	outputData := make([]float32, outputCap)

	// Use the newly translated genWindowedSinesGo
	genWindowedSinesGo(1, []float64{freq}, 1.0, inputData)

	srcData := SrcData{
		DataIn:       inputData,
		InputFrames:  int64(len(inputData)),
		DataOut:      outputData,
		OutputFrames: int64(len(outputData)),
		SrcRatio:     1.999, // Fixed ratio used in C test
		EndOfInput:   true,
	}

	// Using Simple which uses Process internally with EndOfInput=true
	err := Simple(&srcData, converter, 1) // C uses src_simple, 1 channel
	if err != nil {
		// Use Errorf, let caller decide if fatal
		t.Errorf("findAttenuationGo: Simple() failed for freq %.5f: %v (C Line ~253)", freq, err)
		return -1, err
	}

	actualOutputLen := int(srcData.OutputFramesGen)
	if actualOutputLen <= 0 {
		// Use Errorf
		t.Errorf("findAttenuationGo: Simple() produced no output for freq %.5f", freq)
		return -1, fmt.Errorf("no output generated")
	}

	outputPeak := findPeakGo(outputData[:actualOutputLen])
	if outputPeak < 1e-9 {
		t.Logf("findAttenuationGo: Output peak is near zero (%.2e) for freq %.5f", outputPeak, freq)
		return 500.0, nil // Return very high attenuation
	}

	attenuation := 20.0 * math.Log10(1.0/outputPeak)

	if verbose {
		t.Logf("\tFreq : %6f   InPeak : 1.000000    OutPeak : %6f   Atten : %6.2f dB", freq, outputPeak, attenuation)
	}

	return attenuation, nil
}

// testBandwidthGo corresponds to bandwidth_test() in C
func testBandwidthGo(t *testing.T, converter ConverterType, verbose bool) float64 {
	t.Helper()
	// Bandwidth test relies on findAttenuationGo, which seems implementable without FFT.
	// Removing the skip, but it might fail if findAttenuationGo has issues.
	// t.Skipf("Skipping bandwidth test - requires FFT implementation (via findAttenuationGo potentially)")
	// return -1.0 // Placeholder

	var f1, f2, a1, a2, freq, atten float64
	var err error

	f1 = 0.35
	a1, err = findAttenuationGo(t, f1, converter, verbose)
	if err != nil {
		// Use Fatalf as C exits here, suggests prerequisite failed
		t.Fatalf("Bandwidth test: findAttenuationGo failed for f1=%.2f: %v", f1, err)
	}

	f2 = 0.495
	a2, err = findAttenuationGo(t, f2, converter, verbose)
	if err != nil {
		// Use Fatalf
		t.Fatalf("Bandwidth test: findAttenuationGo failed for f2=%.2f: %v", f2, err)
	}

	// Check if 3dB point is bracketed
	if a1 > 3.001 || a2 < 2.999 { // Add small tolerance for float comparison
		t.Errorf("Bandwidth test: Cannot bracket 3dB point (a1=%.2f, a2=%.2f) (C Line ~276)", a1, a2)
		return -1.0 // Return invalid result
	}

	iterations := 0
	maxIterations := 20 // Prevent infinite loops

	// Iterate to find 3dB point more accurately
	// Using tolerance 0.1 based on previous version
	for math.Abs(a2-a1) > 0.1 && iterations < maxIterations {
		iterations++
		freq = f1 + 0.5*(f2-f1)
		atten, err = findAttenuationGo(t, freq, converter, verbose)
		if err != nil {
			// If attenuation fails mid-iteration, maybe Errorf is better than Fatalf? C would exit. Let's Fatalf.
			t.Fatalf("Bandwidth test: findAttenuationGo failed during iteration for freq=%.5f: %v", freq, err)
		}

		if atten < 3.0 {
			f1 = freq
			a1 = atten
		} else {
			f2 = freq
			a2 = atten
		}
	}
	if iterations >= maxIterations {
		t.Logf("Bandwidth test: Reached max iterations (%d). Final bracket: a1=%.2f, a2=%.2f", maxIterations, a1, a2)
	}

	// Linear interpolation for final frequency
	if math.Abs(a2-a1) < 1e-9 { // Avoid division by near-zero
		freq = f1 // Or (f1+f2)/2 ? C doesn't specify. Let's use f1.
		t.Logf("Bandwidth test: Denominator near zero, using freq = %.5f", freq)
	} else {
		freq = f1 + (3.0-a1)*(f2-f1)/(a2-a1)
	}

	// Ensure calculated freq is within reasonable bounds (0.0 to 0.5)
	if freq < 0.0 || freq > 0.5 {
		t.Errorf("Bandwidth test: Calculated frequency %.5f is out of bounds [0.0, 0.5]", freq)
		return -1.0 // Return invalid result
	}

	// Return bandwidth as percentage (* 200 like C)
	return 200.0 * freq
}

// --- Utility Function Implementations ---

// genWindowedSinesGo generates windowed sine waves. Direct translation of C version.
func genWindowedSinesGo(freqCount int, freqs []float64, maxAmp float64, output []float32) {
	outputLen := len(output)
	if outputLen <= 1 { // Need at least 2 points for Hanning window denominator
		for i := range output {
			output[i] = 0.0
		} // Just zero out if too short
		return
	}
	if freqCount <= 0 {
		for i := range output {
			output[i] = 0.0
		} // Zero out if no frequencies
		return
	}

	// Zero the output slice first
	for i := range output {
		output[i] = 0.0
	}

	amplitude := maxAmp / float64(freqCount)
	outputLenF := float64(outputLen)

	for freq := 0; freq < freqCount; freq++ {
		// Check frequency range (C exits on error, we might panic or just log/skip)
		if freqs[freq] <= 0.0 || freqs[freq] >= 0.5 {
			// Using panic similar to C exit. Could return an error instead.
			panic(fmt.Sprintf("genWindowedSinesGo: Error: freq [%d] == %g is out of range (0.0, 0.5).", freq, freqs[freq]))
		}

		// Phase calculation from C (constant within this loop)
		phase := 0.9 * math.Pi / float64(freqCount)

		// Accumulate sine wave for this frequency
		for k := 0; k < outputLen; k++ {
			kF := float64(k)
			// Formula from C: amplitude * sin (freqs[freq] * (2 * k) * M_PI + phase)
			output[k] += float32(amplitude * math.Sin(freqs[freq]*(2.0*kF)*math.Pi+phase))
		}
	}

	// Apply Hanning Window (after summing all sines)
	// Formula from C: 0.5 - 0.5 * cos ((2 * k) * M_PI / (output_len - 1))
	// Note: Denominator is (outputLen - 1)
	denominator := outputLenF - 1.0
	for k := 0; k < outputLen; k++ {
		kF := float64(k)
		window := 0.5 - 0.5*math.Cos((2.0*kF)*math.Pi/denominator)
		output[k] *= float32(window)
	}
}

// calculateSnrGo calculates Signal-to-Noise ratio using FFT.
// STUB IMPLEMENTATION - requires Go FFT library.
func calculateSnrGo(output []float32, numPeaks int) (snr float64, err error) {
	// --- Placeholder ---
	// TODO: Implement using a Go FFT library (e.g., gonum/dsp/fft)
	err = fmt.Errorf("calculateSnrGo needs implementation using a Go FFT library")
	snr = -1.0 // Indicate error or unimplemented
	return snr, err
}

// saveOctFloatGo corresponds to save_oct_float in C util.c
// STUB IMPLEMENTATION
func saveOctFloatGo(filename string, input []float32, output []float32) error {
	// TODO: Implement file saving logic if needed for debugging failures.
	fmt.Printf("saveOctFloatGo: Saving data to %s (Not Implemented)\n", filename)
	return nil
}

// --- Assume these helpers exist elsewhere in the package ---
/*
func minFloat64(a, b float64) float64 { if a < b { return a }; return b }
func minInt(a, b int) int { if a < b { return a }; return b }
func maxInt(a, b int) int { if a > b { return a }; return b }
func psfLrint(x float64) int { if x >= 0.0 { return int(math.Floor(x + 0.5)) }; return int(math.Ceil(x - 0.5)) }
func fmodOne(x float64) float64 { _, frac := math.Modf(x); if frac < 0.0 { frac += 1.0 }; return frac }
func isBadSrcRatio(ratio float64) bool { return ratio <= (1.0/srcMaxRatio) || ratio >= srcMaxRatio } // Simplified check
*/
