package libsamplerate

import (
	"fmt"
	"math"
	// Add other necessary imports for helpers, e.g., "testing" if helpers use t.Helper()
	// Add "gonum.org/v1/gonum/dsp/fourier", "math/cmplx", "sort" if moving calculateSnrGo here
)

// --- Shared Test Helper Functions ---

// genWindowedSinesGo generates windowed sine waves. Direct translation of C version.
func genWindowedSinesGo(freqCount int, freqs []float64, maxAmp float64, output []float32) {
	outputLen := len(output)
	if outputLen <= 1 || freqCount <= 0 {
		for i := range output {
			output[i] = 0.0
		}
		return
	}

	for i := range output {
		output[i] = 0.0
	} // Zero slice

	amplitude := maxAmp / float64(freqCount)
	outputLenF := float64(outputLen)

	for freqIdx := 0; freqIdx < freqCount; freqIdx++ {
		freqVal := freqs[freqIdx]
		if freqVal <= 0.0 || freqVal >= 0.5 {
			panic(fmt.Sprintf("genWindowedSinesGo: Error: freq [%d] == %g is out of range (0.0, 0.5).", freqIdx, freqVal))
		}
		phase := 0.9 * math.Pi / float64(freqCount) // Constant phase from C

		for k := 0; k < outputLen; k++ {
			kF := float64(k)
			output[k] += float32(amplitude * math.Sin(freqVal*(2.0*kF)*math.Pi+phase))
		}
	}

	// Apply Hanning Window
	denominator := outputLenF - 1.0
	for k := 0; k < outputLen; k++ {
		kF := float64(k)
		window := 0.5 - 0.5*math.Cos((2.0*kF)*math.Pi/denominator)
		output[k] *= float32(window)
	}
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

// absInt64 calculates the absolute value of an int64.
func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// minInt64 returns the smaller of two int64 values.
func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
