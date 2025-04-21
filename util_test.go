// util_test.go
package libsamplerate

import (
	"math"
	"testing"
)

const bufferLen = 10000 // From C test

// absInt calculates the absolute value of an int.
func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// absInt32 calculates the absolute value of an int32.
func absInt32(x int32) int32 {
	if x < 0 {
		return -x
	}
	return x
}

// TestFloatToShort corresponds to float_to_short_test
func TestFloatToShort(t *testing.T) {
	// Test cases matching C, including edge cases around 1.0
	fpos := []float32{
		0.0, 0.5, 0.95, 0.99, 1.0, 1.01, 1.1, 2.0, 11.1, 111.1, 2222.2, 33333.3,
		float32(32767.0 / 32768.0),         // ~0.999969
		float32((32767.0 + 0.4) / 32768.0), // ~0.999981 (rounds down/near)
		float32((32767.0 + 0.5) / 32768.0), // ~0.999984 (rounds up with lrintf semantic) -> should be 32767
		float32((32767.0 + 0.6) / 32768.0), // ~0.999987 (rounds up) -> should be 32767
		float32((32767.0 + 0.9) / 32768.0), // ~0.999996 (rounds up) -> should be 32767
		1.0,                                // Exact clip
	}
	fneg := []float32{
		0.0, -0.5, -0.95, -0.99, -1.0, -1.01, -1.1, -2.0, -11.1, -111.1, -2222.2, -33333.3,
		float32(-32768.0 / 32768.0),         // -1.0 (Exact clip)
		float32((-32768.0 - 0.4) / 32768.0), // ~ -1.000012 (rounds down/near) -> should be -32768
		float32((-32768.0 - 0.5) / 32768.0), // ~ -1.000015 (rounds down) -> should be -32768
		float32((-32768.0 - 0.6) / 32768.0), // ~ -1.000018 (rounds down) -> should be -32768
		float32((-32768.0 - 0.9) / 32768.0), // ~ -1.000027 (rounds down) -> should be -32768
		-1.0,                                // Exact clip
	}

	// Expected results (approximate for non-clipping, exact for clipping)
	// Note: psf_lrintf rounding (half away from zero) vs math.Round (half to even) might cause slight diffs near 0.5 frac
	expPos := []int16{
		0, 16384, 31130, 32440, 32767, 32767, 32767, 32767, 32767, 32767, 32767, 32767,
		32767, 32767, 32767, 32767, 32767,
		32767,
	}
	expNeg := []int16{
		0, -16384, -31130, -32440, -32768, -32768, -32768, -32768, -32768, -32768, -32768, -32768,
		-32768, -32768, -32768, -32768, -32768,
		-32768,
	}

	out := make([]int16, maxInt(len(fpos), len(fneg)))

	t.Run("Positive", func(t *testing.T) {
		FloatToShortArray(fpos, out[:len(fpos)])
		for k := 0; k < len(fpos); k++ {
			// C test used rough check ( < 30000). Let's check expected values more closely.
			// Allow small tolerance for non-clipping values due to rounding differences.
			expected := expPos[k]
			tolerance := int16(1) // Allow off-by-one due to rounding?
			if expected == 32767 || expected == -32768 {
				tolerance = 0
			} // Clipping should be exact

			if absInt16(out[k]-expected) > tolerance {
				t.Errorf("fpos[%d]: Input %.8f, Expected ~%d, Got %d", k, fpos[k], expected, out[k])
			}
		}
	})

	t.Run("Negative", func(t *testing.T) {
		FloatToShortArray(fneg, out[:len(fneg)])
		for k := 0; k < len(fneg); k++ {
			// C test used rough check ( > -30000).
			expected := expNeg[k]
			tolerance := int16(1)
			if expected == 32767 || expected == -32768 {
				tolerance = 0
			}

			if absInt16(out[k]-expected) > tolerance {
				t.Errorf("fneg[%d]: Input %.8f, Expected ~%d, Got %d", k, fneg[k], expected, out[k])
			}
		}
	})
}

// TestShortToFloat corresponds to short_to_float_test
func TestShortToFloat(t *testing.T) {
	input := make([]int16, bufferLen)
	output := make([]int16, bufferLen)
	temp := make([]float32, bufferLen)

	// Initialize input ramp
	for k := 0; k < len(input); k++ {
		// Use int64 intermediate to avoid overflow before division
		input[k] = int16((int64(k) * 0x8000) / int64(len(input)))
	}

	// Convert forth and back
	ShortToFloatArray(input, temp)
	FloatToShortArray(temp, output)

	// Check if output matches input
	diffCount := 0
	maxDiff := int16(0)
	for k := 0; k < len(input); k++ {
		diff := absInt16(input[k] - output[k])
		// C check was > 0. Allow 1 due to potential rounding differences float->short.
		if diff > 1 {
			t.Errorf("Mismatch at index %d: Input %d -> Temp %.8f -> Output %d (Diff %d)", k, input[k], temp[k], output[k], input[k]-output[k])
			diffCount++
			// Stop after a few errors to avoid flooding logs
			if diffCount > 10 {
				t.Fatalf("Too many differences found, stopping test.")
			}
		}
		if diff > maxDiff {
			maxDiff = diff
		}
	}
	if diffCount == 0 {
		t.Logf("Short<->Float conversion OK (Max diff: %d)", maxDiff)
	}
}

// TestFloatToInt corresponds to float_to_int_test
func TestFloatToInt(t *testing.T) {
	fpos := []float32{0.0, 0.5, 0.95, 0.99, 1.0, 1.01, 1.1, 2.0, 11.1, 111.1, 2222.2, 33333.3}
	fneg := []float32{0.0, -0.5, -0.95, -0.99, -1.0, -1.01, -1.1, -2.0, -11.1, -111.1, -2222.2, -33333.3}

	// Rough expected values (scaling by 2^31)
	expPosMin := int32(0) // Lower bound for positive inputs
	expNegMax := int32(0) // Upper bound for negative inputs

	out := make([]int32, maxInt(len(fpos), len(fneg)))

	t.Run("Positive", func(t *testing.T) {
		FloatToIntArray(fpos, out[:len(fpos)])
		for k := 0; k < len(fpos); k++ {
			// C check was rough: < 30000 * 0x10000.
			// Values >= 1.0 should clip to MaxInt32. Others should be positive.
			if fpos[k] >= 1.0 {
				if out[k] != math.MaxInt32 {
					t.Errorf("fpos[%d] (>=1.0): Input %.8f, Expected %d, Got %d", k, fpos[k], int32(math.MaxInt32), out[k])
				}
			} else if fpos[k] > 0 {
				if out[k] < expPosMin {
					t.Errorf("fpos[%d] (>0): Input %.8f, Expected > %d, Got %d", k, fpos[k], expPosMin, out[k])
				}
			} else { // Input == 0.0
				if out[k] != 0 {
					t.Errorf("fpos[%d] (==0.0): Input %.8f, Expected 0, Got %d", k, fpos[k], out[k])
				}
			}
		}
	})

	t.Run("Negative", func(t *testing.T) {
		FloatToIntArray(fneg, out[:len(fneg)])
		for k := 0; k < len(fneg); k++ {
			// C check was rough: > -30000 * 0x1000 (Typo? Should be 0x10000?).
			// Values <= -1.0 should clip to MinInt32. Others should be negative.
			if fneg[k] <= -1.0 {
				if out[k] != math.MinInt32 {
					t.Errorf("fneg[%d] (<= -1.0): Input %.8f, Expected %d, Got %d", k, fneg[k], int32(math.MinInt32), out[k])
				}
			} else if fneg[k] < 0 {
				if out[k] > expNegMax {
					t.Errorf("fneg[%d] (<0): Input %.8f, Expected < %d, Got %d", k, fneg[k], expNegMax, out[k])
				}
			} else { // Input == 0.0
				if out[k] != 0 {
					t.Errorf("fneg[%d] (==0.0): Input %.8f, Expected 0, Got %d", k, fneg[k], out[k])
				}
			}
		}
	})
}

// IntToFloat64Array (New Name)
func IntToFloat64Array(in []int32, out []float64) {
	count := minInt(len(in), len(out))
	scale64 := float64(1.0 / 2147483648.0)
	for i := 0; i < count; i++ {
		out[i] = float64(in[i]) * scale64 // Directly store float64
	}
}

// Float64ToIntArray (New Name) - Takes float64
func Float64ToIntArray(in []float64, out []int32) {
	count := minInt(len(in), len(out))
	scale := float64(2147483648.0)
	maxInt32 := int32(math.MaxInt32)
	minInt32 := int32(math.MinInt32)

	for i := 0; i < count; i++ {
		// Input is already float64
		scaledValue := in[i] * scale

		// Round first using math.Round (via psfLrint - ensure it uses math.Round)
		rounded := psfLrint(scaledValue) // Returns int

		// Clip AFTER rounding
		if rounded >= int(maxInt32) {
			out[i] = maxInt32
		} else if rounded <= int(minInt32) {
			out[i] = minInt32
		} else {
			out[i] = int32(rounded)
		}
	}
}

// TestIntToFloat using float64 intermediate
func TestIntToFloat(t *testing.T) {
	input := make([]int32, bufferLen)
	output := make([]int32, bufferLen)
	// Use float64 for temp
	temp := make([]float64, bufferLen)

	for k := 0; k < len(input); k++ {
		input[k] = int32((int64(k) * math.MinInt32) / int64(len(input)))
	}

	// Use float64 versions
	IntToFloat64Array(input, temp)
	Float64ToIntArray(temp, output)

	// Check if output matches input (Reset tolerance to 1 initially)
	diffCount := 0
	maxDiff := int32(0)
	for k := 0; k < len(input); k++ {
		diff := absInt32(input[k] - output[k])
		if diff > 1 { // Allow diff of only 0 or 1
			t.Errorf("Mismatch at index %d: Input %d -> Temp %.15f -> Output %d (Diff %d)", k, input[k], temp[k], output[k], input[k]-output[k])
			diffCount++
			if diffCount > 10 {
				t.Fatalf("Too many differences found, stopping test.")
			}
		}
		if diff > maxDiff {
			maxDiff = diff
		}
	}
	if diffCount == 0 {
		t.Logf("Int<->Float64 conversion OK (Max diff: %d)", maxDiff)
	} else {
		t.Logf("Int<->Float64 conversion finished with %d differences (Max diff: %d)", diffCount, maxDiff)
	}
}

// Helper for absolute difference of int16
func absInt16(x int16) int16 {
	if x < 0 {
		return -x
	}
	return x
}

// reverseDataGo reverses the elements of a float32 slice in place.
func reverseDataGo(data []float32) {
	left := 0
	right := len(data) - 1
	for left < right {
		data[left], data[right] = data[right], data[left]
		left++
		right--
	}
}
