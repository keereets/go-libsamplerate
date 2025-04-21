// throughput_test.go

//# Run only benchmarks matching "Throughput"
//go test -bench BenchmarkConverterThroughput -benchmem
//
//# Run all benchmarks in the package
//# go test -bench . -benchmem
//

package libsamplerate

import (
	"fmt"
	"math"
	"runtime"
	"testing"
	"time"
	// Assumes types like ConverterType, SrcData, New, Simple etc. are defined
	// Assumes genWindowedSinesGo is defined (from snr_bw_test.go or util)
)

const (
	bufferLenThroughput = 1 << 16 // 65536
)

// Package-level buffers, initialized once
var (
	inputBench  []float32
	outputBench []float32
)

// init initializes the benchmark buffers.
func init() {
	fmt.Println("Initializing benchmark buffers...")
	inputBench = make([]float32, bufferLenThroughput)
	// Calculate max output capacity needed for the lowest ratio tested (0.99)
	// Simple API processes all input, so output is roughly inputLen * ratio
	// For ratio 0.99, output len ~ input len. Let's add margin.
	maxOut := bufferLenThroughput + 1000 // Sufficient capacity
	outputBench = make([]float32, maxOut)

	// Generate input signal (matches C main)
	freq := 0.01
	// Assuming genWindowedSinesGo is available in this package
	genWindowedSinesGo(1, []float64{freq}, 1.0, inputBench)
	fmt.Printf("Initialized input buffer (%d samples).\n", len(inputBench))
	fmt.Printf("Initialized output buffer (%d samples capacity).\n", len(outputBench))
}

// roundFloatToInt64 mimics C's lrint (round half away from zero)
func roundFloatToInt64(x float64) int64 {
	if x >= 0.0 {
		return int64(math.Floor(x + 0.5))
	}
	return int64(math.Ceil(x - 0.5))
}

// getRuntimeCPU provides basic CPU info available from Go runtime.
func getRuntimeCPU() string {
	return fmt.Sprintf("%s (%d cores)", runtime.GOARCH, runtime.NumCPU())
}

// BenchmarkConverterThroughput runs throughput tests for different converters.
func BenchmarkConverterThroughput(b *testing.B) {

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

	fmt.Printf("\n    CPU name : %s\n", getRuntimeCPU())
	fmt.Println("\n" +
		"    Converter                        Result (Frames/sec)\n" +
		"    -----------------------------------------------------------")

	const srcRatio = 0.99 // Fixed ratio used in C test

	for _, tt := range convertersToTest {
		convTest := tt // Capture range variable

		if !convTest.enabled {
			b.Logf("Skipping benchmark for %s (disabled in package)", convTest.name)
			continue
		}

		b.Run(convTest.name, func(b *testing.B) {
			// Setup SRC_DATA (do this outside the loop)
			// Check if output buffer has enough capacity for this ratio
			expectedOutputFramesRough := int64(float64(len(inputBench)) * srcRatio)
			if expectedOutputFramesRough > int64(len(outputBench)) {
				b.Fatalf("Output buffer capacity (%d) too small for expected output (%d)", len(outputBench), expectedOutputFramesRough)
			}

			srcData := SrcData{
				DataIn:       inputBench,
				InputFrames:  int64(len(inputBench)),
				DataOut:      outputBench,
				OutputFrames: int64(len(outputBench)), // Provide full capacity
				SrcRatio:     srcRatio,
				// EndOfInput is handled by Simple API
			}
			var lastErr error
			var lastInputUsed int64
			var lastOutputGen int64

			// Initial warm-up / pause like C test?
			// This is unusual for Go benchmarks, but let's include it.
			// Run once before resetting timer to ensure initialization/caches are warm.
			// Also include the sleep if mimicking C closely.
			b.StopTimer()                                        // Stop timer during sleep/warmup
			time.Sleep(1 * time.Second)                          // Reduced sleep from C's 2s
			errWarmup := Simple(&srcData, convTest.converter, 1) // Warmup run
			if errWarmup != nil {
				b.Fatalf("Warmup run failed: %v", errWarmup)
			}
			time.Sleep(1 * time.Second) // Sleep after warmup before timing
			b.StartTimer()              // Start timer *just* before the benchmark loop

			// The benchmark loop provided by 'testing.B'
			// b.N is determined automatically by the framework.
			for i := 0; i < b.N; i++ {
				// Reset Used/Gen fields if necessary (Simple might reuse the struct)
				// However, Simple creates a new state each time, so these should be
				// correctly populated based on the single call within Simple.
				// Let's assume we just need to capture the result after the call.

				lastErr = Simple(&srcData, convTest.converter, 1) // Run the conversion
				if lastErr != nil {
					// Stop benchmark immediately on error
					b.Fatalf("Simple() failed during benchmark: %v", lastErr)
				}
				// Store results from the *last* iteration for checks after the loop
				lastInputUsed = srcData.InputFramesUsed
				lastOutputGen = srcData.OutputFramesGen
			}

			b.StopTimer() // Stop timer after the loop

			// --- Post-Benchmark Checks (Optional but Recommended) ---
			// These checks use the results from the *last* iteration (i = b.N-1)

			// Check for errors during the last run (already done via b.Fatalf)

			// Check input used (Simple should consume all input)
			if lastInputUsed != int64(len(inputBench)) {
				b.Errorf("Input used mismatch after loop: Got %d, Expected %d", lastInputUsed, len(inputBench))
			}

			// Check output length consistency (using tolerance from C test)
			expectedOutputF := srcData.SrcRatio * float64(lastInputUsed) // Use actual used input for check
			if math.Abs(float64(lastOutputGen)-expectedOutputF) > 2.0 {
				b.Errorf("Output count mismatch after loop: Got %d, Expected %.1f +/- 2.0", lastOutputGen, expectedOutputF)
			}

			// --- Report Throughput Metric ---
			// Calculate total output frames generated over b.N runs
			totalOutputFrames := int64(b.N) * lastOutputGen
			// Calculate throughput in output frames per second
			throughput := float64(totalOutputFrames) / b.Elapsed().Seconds()

			// Report the custom metric (frames/sec)
			// The benchmark framework will also report ns/op automatically.
			b.ReportMetric(throughput, "frames/s")

			// Optional: Log the throughput like C test for immediate visibility
			// Note: This might be redundant with the final benchmark summary table.
			// b.Logf("%s : %.2f sec, %.0f frames/s", convTest.name, b.Elapsed().Seconds(), throughput)

		}) // End b.Run for converter
	} // End loop over converters

	// C code had single_run vs multi_run. Go benchmark framework handles multiple runs via b.N.
	// The final summary table is automatically printed by `go test -bench`.
	// We don't need the manual multi_run logic or best_throughput tracking.

	fmt.Println("\n" +
		"            Duration reported by benchmark is total for N runs.\n" +
		"            Throughput ('frames/s') is calculated total output frames / total duration.\n" +
		"            Benchmark also reports 'ns/op' (nanoseconds per call to Simple API).")

}
