//
// Copyright (c) 2025, Antonio Chirizzi <antonio.chirizzi@gmail.com>
// All rights reserved.
//
// This code is released under 3-clause BSD license. Please see the
// file LICENSE
//

package libsamplerate

import (
	"fmt"
	"math"
	"sync"
	"testing"
	// Assumes types, constants, Simple are defined
	// Assumes genWindowedSinesGo is defined (e.g., in test_utils.go)
)

// TestSimpleConcurrentIndependent tests running src_simple equivalent concurrently
// on separate data using separate implicit states. This pattern SHOULD be safe.
func TestSimpleConcurrentIndependent(t *testing.T) {
	const numGoroutines = 30
	const inputFrames = 8192    // Use a moderate buffer size
	const channels = 1          // Keep it simple with mono
	const quality = SincFastest // Choose a non-trivial converter
	const srcRatio = 1.2345     // Choose an arbitrary ratio

	// Use WaitGroup to wait for all goroutines to finish
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	// Use a channel to collect errors from goroutines
	errChan := make(chan error, numGoroutines)

	t.Logf("Starting %d concurrent independent Simple conversions...", numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(gID int) {
			defer wg.Done() // Signal this goroutine is done when it exits

			// --- Prepare Independent Data for this Goroutine ---
			logPrefix := fmt.Sprintf("Goroutine %d: ", gID)

			// Create unique input data (sine wave with slightly different freq)
			//freq := 0.1 + (float64(gID) * 0.05) // e.g., 0.1, 0.15, 0.2

			// Spread numGoroutines frequencies between 0.1 and 0.45
			var freq float64
			if numGoroutines > 1 {
				freq = 0.1 + (float64(gID) * (0.45 - 0.1) / float64(numGoroutines-1))
			} else {
				freq = 0.1 // Default for single goroutine case
			}
			// Ensure freq stays strictly below 0.5 due to floating point potential
			if freq >= 0.5 {
				freq = 0.499
			}
			if freq <= 0.0 {
				freq = 0.001
			} // Ensure > 0.0

			inputData := make([]float32, inputFrames*channels)
			genWindowedSinesGo(1, []float64{freq}, 0.8, inputData) // Use helper

			// Allocate separate output buffer for this goroutine
			outputFramesEstimate := int64(math.Ceil(float64(len(inputData)/channels)*srcRatio)) + 20
			outputData := make([]float32, outputFramesEstimate*int64(channels))

			// Prepare SrcData specific to this goroutine
			srcData := SrcData{
				DataIn:       inputData,
				InputFrames:  int64(len(inputData) / channels),
				DataOut:      outputData,
				OutputFrames: outputFramesEstimate, // Capacity
				SrcRatio:     srcRatio,
				// EndOfInput is set by Simple
			}

			// --- Execute Simple Conversion ---
			err := Simple(&srcData, quality, channels)

			// --- Check Results & Report Errors via Channel ---
			if err != nil {
				errChan <- fmt.Errorf("%s Simple() failed: %w", logPrefix, err)
				return // Exit goroutine on error
			}

			// Basic sanity checks
			if srcData.InputFramesUsed != srcData.InputFrames {
				errChan <- fmt.Errorf("%s did not consume all input: Used %d, Expected %d", logPrefix, srcData.InputFramesUsed, srcData.InputFrames)
			}
			if srcData.OutputFramesGen <= 0 {
				errChan <- fmt.Errorf("%s generated zero output frames", logPrefix)
			}
			// Could add more checks here if desired (e.g., rough output length)

			// If verbose, log success for this goroutine? Optional.
			// fmt.Printf("%s Conversion successful (In: %d, Out: %d)\n", logPrefix, srcData.InputFramesUsed, srcData.OutputFramesGen)

		}(i) // Pass loop variable i to the goroutine
	}

	// Wait for all goroutines to complete and close the error channel
	go func() {
		wg.Wait()
		close(errChan)
	}()

	// Check for any errors reported by the goroutines
	hadErrors := false
	for err := range errChan {
		t.Errorf("%v", err) // Log each error using t.Errorf
		hadErrors = true
	}

	if !hadErrors {
		t.Logf("All %d concurrent independent Simple conversions completed successfully.", numGoroutines)
	}
}
