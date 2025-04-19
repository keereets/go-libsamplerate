// common.go
package libsamplerate

import (
	"math"
)

// --- Core Types ---

// SrcData corresponds to SRC_DATA in samplerate.h
type SrcData struct {
	// DataIn is the input buffer.
	DataIn []float32 // const float *data_in

	// DataOut is the output buffer.
	DataOut []float32 // float *data_out

	// InputFrames is the number of frames available in DataIn.
	InputFrames int64 // long input_frames

	// OutputFrames is the maximum number of frames that can be written to DataOut.
	OutputFrames int64 // long output_frames

	// InputFramesUsed will be set by the processing function to the number
	// of frames actually read from DataIn.
	InputFramesUsed int64 // long input_frames_used

	// OutputFramesGen will be set by the processing function to the number
	// of frames actually written to DataOut.
	OutputFramesGen int64 // long output_frames_gen

	// EndOfInput should be set to true if this is the last block
	// of input data, otherwise false.
	EndOfInput bool // int end_of_input (using bool)

	// SrcRatio is the desired conversion ratio (output_sample_rate / input_sample_rate).
	SrcRatio float64 // double src_ratio
}

// CallbackFunc is the Go equivalent of src_callback_t.
// It takes the user-provided data and should return a slice of float32 audio data
// and the number of frames in that slice. Returning an error is also advisable.
// The C version returning long and modifying a float** is tricky to map directly.
// This signature provides a more Go-idiomatic way for the callback to provide data.
type CallbackFunc func(userData interface{}) (data []float32, framesRead int64, err error)

// srcState holds the internal state for a converter instance.
// Corresponds to SRC_STATE_tag in common.h
type srcState struct {
	vt *srcStateVT // Pointer to the virtual method table for this converter type

	lastRatio    float64 // Previously used ratio
	lastPosition float64 // Position across buffer boundaries (0.0 to < 1.0)

	errCode  ErrorCode // Last error encountered (internal)
	channels int       // Number of channels

	mode Mode // Current operating mode (Process or Callback)

	// --- Callback Mode Data ---
	callbackFunc     CallbackFunc // User-provided function to get input data
	userCallbackData interface{}  // User data passed to the callback function
	savedFrames      int64        // Frames remaining from the last callback read
	savedData        []float32    // Slice pointing to remaining data from last callback

	// --- Converter Specific Data ---
	// Use interface{} to hold the specific filter state (e.g., *sincFilter)
	privateData interface{}
}

// srcStateVT holds the function pointers for a specific converter type.
// Corresponds to SRC_STATE_VT_tag in common.h
type srcStateVT struct {
	variProcess  func(state *srcState, data *SrcData) ErrorCode
	constProcess func(state *srcState, data *SrcData) ErrorCode
	reset        func(state *srcState)
	copy         func(state *srcState) *srcState // Returns a deep copy
	close        func(state *srcState)           // Frees associated resources (if any beyond GC)
}

// --- Constants and Enums ---

// ConverterType identifies the sample rate conversion algorithm.
type ConverterType int

const (
	SincBestQuality   ConverterType = 0 // SRC_SINC_BEST_QUALITY
	SincMediumQuality ConverterType = 1 // SRC_SINC_MEDIUM_QUALITY
	SincFastest       ConverterType = 2 // SRC_SINC_FASTEST
	ZeroOrderHold     ConverterType = 3 // SRC_ZERO_ORDER_HOLD
	Linear            ConverterType = 4 // SRC_LINEAR
)

// Mode identifies the operational mode of the converter state.
type Mode int

const (
	ModeProcess  Mode = 0 // SRC_MODE_PROCESS
	ModeCallback Mode = 1 // SRC_MODE_CALLBACK
)

// ErrorCode defines the possible error values.
type ErrorCode int

const (
	ErrNoError ErrorCode = iota // SRC_ERR_NO_ERROR
	ErrMallocFailed
	ErrBadState
	ErrBadData
	ErrBadDataPtr
	ErrNoPrivate
	ErrBadSrcRatio
	ErrBadProcPtr // Internal error
	ErrShiftBits  // Internal error
	ErrFilterLen  // Internal error
	ErrBadConverter
	ErrBadChannelCount
	ErrSincBadBufferLen    // Internal error
	ErrSizeIncompatibility // Internal error
	ErrBadPrivPtr          // Internal error
	ErrBadSincState        // Specific to sinc
	ErrDataOverlap
	ErrBadCallback
	ErrBadMode
	ErrNullCallback
	ErrNoVariableRatio       // Specific converter limitation
	ErrSincPrepareDataBadLen // Internal Sinc error
	ErrBadInternalState      // Catch-all internal

	// ErrMaxError // Placeholder for the end
)

// Useful constants from common.h/config.h
const (
	srcMaxRatio     = 256.0 // SRC_MAX_RATIO
	srcMaxRatioStr  = "256" // SRC_MAX_RATIO_STR (less useful in Go)
	srcMinRatioDiff = 1e-20 // SRC_MIN_RATIO_DIFF
)

// --- Internal Helper Functions (from common.h) ---

// // psfLrint rounds a float64 to the nearest integer (like C lrint).
func psfLrint(x float64) int {
	// Go's math.Round rounds half to even, C's lrint typically rounds half away from zero.
	// Depending on requirements, math.Round might be sufficient, or a custom impl needed.
	// Let's use math.Round for now. Add 0.0 to handle -0.0 case if needed.
	return int(math.Round(x + 0.0))
}

// psfLrintf rounds a float32 to the nearest integer (like C lrintf).
func psfLrintf(x float32) int {
	return int(math.Round(float64(x) + 0.0))
}

//// psfLrintf rounds a float32 to the nearest integer, rounding half away from zero.
//// Mimics C lrintf behavior.
//func psfLrintf(x float32) int {
//	xf64 := float64(x) // Convert to float64 for calculation
//	if xf64 >= 0.0 {
//		return int(math.Floor(xf64 + 0.5))
//	}
//	return int(math.Ceil(xf64 - 0.5))
//}

// fmodOne calculates x mod 1.0, ensuring the result is in [0.0, 1.0).
func fmodOne(x float64) float64 {
	// Use standard math.Mod
	res := math.Mod(x, 1.0)
	// Handle negative results from math.Mod
	if res < 0.0 {
		res += 1.0
	}
	// Ensure result is strictly less than 1.0 due to potential precision issues near integers
	if res >= 1.0 {
		res = 0.0
	}
	return res
	/* // Alternative using psfLrint, closer to C code but maybe less robust?
	   res := x - float64(psfLrint(x))
	   if res < 0.0 {
	       return res + 1.0
	   }
	   return res
	*/
}

// IsValidRatio checks if the ratio is within the library's supported range.
// Corresponds to src_is_valid_ratio macro logic. Public version in samplerate.go
func isValidRatio(ratio float64) bool {
	return !(ratio < (1.0/srcMaxRatio) || ratio > srcMaxRatio)
}

// isBadSrcRatio is the inverse check used internally.
func isBadSrcRatio(ratio float64) bool {
	return ratio < (1.0/srcMaxRatio) || ratio > srcMaxRatio
}

// Simple min/max helpers (Go 1.21+ has math.Min/Max)
// Assuming Go < 1.21 for broader compatibility for now
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minFloat64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
