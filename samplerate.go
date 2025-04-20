// samplerate.go
package libsamplerate

import (
	"fmt"
	"math"
)

// --- Public API ---

// Converter is the interface representing an active sample rate converter instance.
// Using an interface provides better encapsulation than exposing *srcState directly.
type Converter interface {
	// Process converts audio data according to the parameters in SrcData.
	Process(data *SrcData) error
	// Reset resets the internal converter state.
	Reset() error
	// SetRatio sets a new conversion ratio.
	SetRatio(newRatio float64) error
	// GetChannels returns the number of channels the converter was configured for.
	GetChannels() int
	// Close releases any resources associated with the converter.
	// It is important to call this when done, especially if C memory or
	// OS resources were hypothetically involved (though less likely in pure Go).
	Close() error
	// Error returns the last error encountered by the converter.
	// Note: Idiomatic Go prefers functions return errors directly. This mirrors the C API.
	LastError() error
	// Clone creates a new converter instance with the same internal state.
	Clone() (Converter, error)
}

// Compile-time check to ensure srcState implements Converter
var _ Converter = (*srcState)(nil)

// New creates a new sample rate converter.
func New(converterType ConverterType, channels int) (Converter, error) {
	// Internal function psrcSetConverter handles the actual creation logic
	state, errCode := psrcSetConverter(converterType, channels)
	if errCode != ErrNoError {
		return nil, mapError(errCode) // Convert ErrorCode to Go error
	}
	// The concrete type *srcState implements the Converter interface
	return state, nil
}

// Simple performs a one-shot conversion. Useful for single blocks of audio.
func Simple(data *SrcData, converterType ConverterType, channels int) error {
	if data == nil {
		return mapError(ErrBadData)
	}
	state, err := New(converterType, channels)
	if err != nil {
		return err // Already a Go error
	}

	// Mark as end of input for simple mode
	data.EndOfInput = true

	err = state.Process(data)

	// Ensure Close is called, although GC handles memory for pure Go objects.
	// Using Close explicitly is good practice if resources could be held.
	_ = state.Close() // Ignore Close error in simple mode? Or return it?

	return err // Return the error from Process
}

// CallbackNew creates a new converter using a callback function to supply input data.
func CallbackNew(cbFunc CallbackFunc, converterType ConverterType, channels int, userData interface{}) (Converter, error) {
	if cbFunc == nil {
		return nil, mapError(ErrBadCallback)
	}

	state, errCode := psrcSetConverter(converterType, channels)
	if errCode != ErrNoError {
		return nil, mapError(errCode)
	}

	// Reset state specifically for callback mode
	if err := state.Reset(); err != nil {
		_ = state.Close() // Clean up partially created state
		return nil, fmt.Errorf("failed to reset state for callback mode: %w", err)
	}

	state.mode = ModeCallback
	state.callbackFunc = cbFunc
	state.userCallbackData = userData
	state.savedData = nil // Ensure initially nil
	state.savedFrames = 0

	return state, nil
}

// CallbackRead reads converted data when using callback mode.
func CallbackRead(c Converter, ratio float64, framesToRead int64, outData []float32) (framesRead int64, err error) {
	state, ok := c.(*srcState)
	if !ok || state == nil {
		return 0, mapError(ErrBadState)
	}

	if framesToRead <= 0 {
		return 0, nil // Nothing to read
	}
	if state.mode != ModeCallback {
		state.errCode = ErrBadMode
		return 0, mapError(ErrBadMode)
	}
	if state.callbackFunc == nil {
		state.errCode = ErrNullCallback
		return 0, mapError(ErrNullCallback)
	}
	if isBadSrcRatio(ratio) {
		state.errCode = ErrBadSrcRatio
		return 0, mapError(ErrBadSrcRatio)
	}
	if len(outData) < int(framesToRead)*state.channels {
		// Not enough space in output buffer
		// This check wasn't explicit in C, but good practice in Go
		return 0, fmt.Errorf("output buffer too small: need %d, got %d", int(framesToRead)*state.channels, len(outData))
	}

	var srcData SrcData
	srcData.SrcRatio = ratio
	srcData.DataOut = outData
	srcData.OutputFrames = framesToRead

	// Use saved data from previous call, if any
	srcData.DataIn = state.savedData
	srcData.InputFrames = state.savedFrames
	srcData.EndOfInput = false // Assume not end unless callback indicates

	totalOutputFramesGen := int64(0)
	currentOutPos := 0 // Position in the user's outData buffer

	for totalOutputFramesGen < framesToRead {
		// Need more input data?
		if srcData.InputFrames == 0 && !srcData.EndOfInput {
			// Call the user's callback function
			inputData, inputFrames, cbErr := state.callbackFunc(state.userCallbackData)
			if cbErr != nil {
				// Propagate callback error
				state.errCode = ErrBadCallback // Or a more specific error?
				return totalOutputFramesGen, fmt.Errorf("callback error: %w", cbErr)
			}
			if inputFrames == 0 || len(inputData) == 0 {
				// Callback signalled end of input
				srcData.EndOfInput = true
			}
			srcData.DataIn = inputData
			srcData.InputFrames = inputFrames
		}

		// Prepare output slice for this process call
		// Ensure we don't try to write past the end of the user's buffer
		remainingFrames := framesToRead - totalOutputFramesGen
		if int(remainingFrames)*state.channels > len(srcData.DataOut[currentOutPos:]) {
			remainingFrames = int64(len(srcData.DataOut[currentOutPos:]) / state.channels)
		}
		if remainingFrames <= 0 {
			break // No more space in output buffer
		}
		srcData.DataOut = outData[currentOutPos:] // Slice for current output position
		srcData.OutputFrames = remainingFrames    // Max frames for this iteration

		srcData.InputFramesUsed = 0 // Reset before process
		srcData.OutputFramesGen = 0 // Reset before process

		// Call the core process function (temporarily switch mode if needed - design choice)
		// For simplicity here, assume Process handles the internal mode correctly
		// or that the VT functions are called directly based on last_ratio vs ratio.
		processErr := c.Process(&srcData) // Use the interface method
		if processErr != nil {
			// Process function encountered an error
			state.errCode = mapGoErrorToCode(processErr) // Map back if possible
			return totalOutputFramesGen, processErr
		}

		// Update input buffer position
		processedInputSamples := srcData.InputFramesUsed * int64(state.channels)
		if processedInputSamples > int64(len(srcData.DataIn)) {
			// Should not happen if Process works correctly
			state.errCode = ErrBadInternalState
			return totalOutputFramesGen, mapError(ErrBadInternalState)
		}
		srcData.DataIn = srcData.DataIn[processedInputSamples:]
		srcData.InputFrames -= srcData.InputFramesUsed

		// Update output buffer position
		generatedOutputSamples := srcData.OutputFramesGen * int64(state.channels)
		currentOutPos += int(generatedOutputSamples)
		totalOutputFramesGen += srcData.OutputFramesGen

		// Check termination conditions
		if srcData.EndOfInput && srcData.OutputFramesGen == 0 {
			// End of input reached and no more output could be generated
			break
		}
		if totalOutputFramesGen >= framesToRead {
			break // We have generated enough frames
		}
	}

	// Save remaining input data for the next call
	state.savedData = srcData.DataIn
	state.savedFrames = srcData.InputFrames
	state.errCode = ErrNoError // Clear internal error on success

	return totalOutputFramesGen, nil
}

// --- Implement Converter Interface for *srcState ---

// Process wraps the internal processing logic.
func (state *srcState) Process(data *SrcData) error {
	if state == nil {
		return mapError(ErrBadState)
	}
	if state.mode != ModeProcess && state.mode != ModeCallback { // Allow callback internals to call process
		state.errCode = ErrBadMode
		return mapError(ErrBadMode)
	}
	if data == nil {
		state.errCode = ErrBadData
		return mapError(ErrBadData)
	}
	if (data.InputFrames > 0 && len(data.DataIn) == 0) || (data.OutputFrames > 0 && len(data.DataOut) == 0) {
		state.errCode = ErrBadDataPtr
		return mapError(ErrBadDataPtr)
	}
	// Check for overlap - complex pointer math in C, tricky with slices.
	// Basic check: If slices point into the same underlying array and overlap.
	// This requires unsafe access in Go to check reliably. Skip for now, maybe add later if critical.
	/*
		if overlaps(data.DataIn, data.DataOut) {
		    state.errCode = ErrDataOverlap
		    return mapError(ErrDataOverlap)
		}
	*/

	if isBadSrcRatio(data.SrcRatio) {
		state.errCode = ErrBadSrcRatio
		return mapError(ErrBadSrcRatio)
	}

	// Ensure counts are non-negative
	if data.InputFrames < 0 {
		data.InputFrames = 0
	}
	if data.OutputFrames < 0 {
		data.OutputFrames = 0
	}

	data.InputFramesUsed = 0
	data.OutputFramesGen = 0

	// Handle initial ratio state
	if state.lastRatio < (1.0 / srcMaxRatio) { // Use near-zero check
		state.lastRatio = data.SrcRatio
	}

	// Choose constant or variable ratio processing function from VT
	var errCode ErrorCode
	if state.vt == nil {
		errCode = ErrBadState // VT not initialized
	} else if math.Abs(state.lastRatio-data.SrcRatio) < 1e-15 {
		if state.vt.constProcess == nil {
			errCode = ErrBadProcPtr
		} else {
			errCode = state.vt.constProcess(state, data)
		}
	} else {
		if state.vt.variProcess == nil {
			errCode = ErrBadProcPtr
		} else {
			errCode = state.vt.variProcess(state, data)
		}
	}

	state.errCode = errCode  // Store internal code
	return mapError(errCode) // Return Go error
}

// Reset resets the converter state via the VT.
func (state *srcState) Reset() error {
	if state == nil {
		return mapError(ErrBadState)
	}
	if state.vt == nil || state.vt.reset == nil {
		state.errCode = ErrBadProcPtr
		return mapError(ErrBadProcPtr)
	}
	state.vt.reset(state)

	// Reset common fields (as done in C src_reset)
	state.lastPosition = 0.0
	state.lastRatio = 0.0 // Reset last known ratio
	state.savedData = nil
	state.savedFrames = 0
	state.errCode = ErrNoError

	return nil
}

// SetRatio updates the target conversion ratio.
func (state *srcState) SetRatio(newRatio float64) error {
	if state == nil {
		return mapError(ErrBadState)
	}
	if isBadSrcRatio(newRatio) {
		state.errCode = ErrBadSrcRatio
		return mapError(ErrBadSrcRatio)
	}
	state.lastRatio = newRatio // Update the target ratio
	// The process function will handle the change on the next call
	state.errCode = ErrNoError
	return nil
}

// GetChannels returns the configured channel count.
func (state *srcState) GetChannels() int {
	if state == nil {
		return 0 // Or return an error? C API returns negative error.
	}
	return state.channels
}

// Close calls the converter-specific close function via the VT.
func (state *srcState) Close() error {
	if state == nil {
		return nil // Or ErrBadState? C returns NULL.
	}
	if state.vt != nil && state.vt.close != nil {
		state.vt.close(state) // Allow specific cleanup
	}
	// Help GC by nil-ing out fields, especially slices and interfaces
	state.privateData = nil
	state.savedData = nil
	state.vt = nil              // Prevent further use
	state.errCode = ErrBadState // Mark as closed
	// The state itself will be GC'd if no external references remain.
	return nil
}

// LastError returns the last error encountered.
func (state *srcState) LastError() error {
	if state == nil {
		return mapError(ErrNoError) // Or ErrBadState?
	}
	return mapError(state.errCode)
}

// Clone creates a deep copy of the converter state.
func (state *srcState) Clone() (Converter, error) {
	if state == nil {
		return nil, mapError(ErrBadState)
	}
	if state.vt == nil || state.vt.copy == nil {
		state.errCode = ErrBadProcPtr
		return nil, mapError(ErrBadProcPtr)
	}

	// The copy function in VT is responsible for deep copying
	// including the privateData.
	newState := state.vt.copy(state)
	if newState == nil {
		// If copy failed (e.g., malloc in C context, or resource exhaustion)
		// Try to map potential internal error code set by copy()
		err := mapError(state.errCode)
		if state.errCode == ErrNoError { // If copy didn't set an error, assume allocation failed
			err = mapError(ErrMallocFailed)
		}
		return nil, err
	}

	return newState, nil // Return the new state as the Converter interface
}

// Version returns the library version string.
func Version() string {
	// Match the C function, potentially update string over time
	return "Go libsamplerate translation (based on C version 0.2.2)"
}

// IsValidRatio checks if a conversion ratio is valid.
func IsValidRatio(ratio float64) bool {
	return isValidRatio(ratio) // Use internal helper
}

// StrError converts an error code to a human-readable string.
func StrError(err error) string {
	// Try to map Go error back to ErrorCode first
	code := mapGoErrorToCode(err)

	switch code {
	case ErrNoError:
		return "No error."
	case ErrMallocFailed:
		return "Memory allocation failed." // More generic in Go
	case ErrBadState:
		return "Invalid converter state."
	case ErrBadData:
		return "Invalid SrcData provided."
	case ErrBadDataPtr:
		return "Input or output buffer is nil/empty."
	case ErrNoPrivate:
		return "Internal error: No private data." // Should not happen with interface{}
	case ErrBadSrcRatio:
		return fmt.Sprintf("SRC ratio outside [1/%s, %s] range.", srcMaxRatioStr, srcMaxRatioStr)
	case ErrBadSincState:
		return "Sinc Error: Process called after end of input without reset." // Example
	case ErrBadProcPtr:
		return "Internal error: Invalid processing function."
	case ErrShiftBits:
		return "Internal error: SHIFT_BITS too large."
	case ErrFilterLen:
		return "Internal error: Filter length too large."
	case ErrBadConverter:
		return "Invalid converter type specified."
	case ErrBadChannelCount:
		return "Channel count must be >= 1."
	case ErrSincBadBufferLen:
		return "Internal error: Bad buffer length calculation."
	case ErrSizeIncompatibility:
		return "Internal error: Data size incompatibility."
	case ErrBadPrivPtr:
		return "Internal error: Private data pointer is invalid." // Less likely with interface{}
	case ErrDataOverlap:
		return "Input and output data arrays overlap." // Harder to detect reliably in Go
	case ErrBadCallback:
		return "Invalid callback function provided."
	case ErrBadMode:
		return "Calling mode differs from initialization mode."
	case ErrNullCallback:
		return "Internal error: Callback function is nil during callback processing."
	case ErrNoVariableRatio:
		return "Converter does not support variable ratios."
	case ErrSincPrepareDataBadLen:
		return "Internal error: Bad length in Sinc prepare_data."
	case ErrBadInternalState:
		return "Internal error: Inconsistent state detected."
	default:
		// If it wasn't one of the known codes, return the original error message
		return err.Error()
	}
}

// mapError converts internal ErrorCode to Go error type.
// Returns nil for ErrNoError.
func mapError(code ErrorCode) error {
	if code == ErrNoError {
		return nil
	}
	// Use fmt.Errorf to wrap the error code description
	// Calling StrError(mapError(code)) would be circular
	// Instead, get the base string description directly
	// This requires a function similar to StrError but only returning the base msg
	msg := getErrorString(code)
	if msg == "" {
		msg = "Unknown error"
	}
	// Optionally create custom error types here
	return fmt.Errorf("libsamplerate error %d: %s", code, msg)
}

// getErrorString returns the base message for an ErrorCode.
func getErrorString(code ErrorCode) string {
	// This switch is duplicated from StrError, consider refactoring later
	switch code {
	case ErrNoError:
		return "No error."
	case ErrMallocFailed:
		return "Memory allocation failed."
	case ErrBadState:
		return "Invalid converter state."
	case ErrBadData:
		return "Invalid SrcData provided."
	case ErrBadDataPtr:
		return "Input or output buffer is nil/empty."
	case ErrNoPrivate:
		return "Internal error: No private data."
	case ErrBadSrcRatio:
		return fmt.Sprintf("SRC ratio outside [1/%s, %s] range.", srcMaxRatioStr, srcMaxRatioStr)
	case ErrBadSincState:
		return "Sinc Error: Process called after end of input without reset."
	case ErrBadProcPtr:
		return "Internal error: Invalid processing function."
	case ErrShiftBits:
		return "Internal error: SHIFT_BITS too large."
	case ErrFilterLen:
		return "Internal error: Filter length too large."
	case ErrBadConverter:
		return "Invalid converter type specified."
	case ErrBadChannelCount:
		return "Channel count must be >= 1."
	case ErrSincBadBufferLen:
		return "Internal error: Bad buffer length calculation."
	case ErrSizeIncompatibility:
		return "Internal error: Data size incompatibility."
	case ErrBadPrivPtr:
		return "Internal error: Private data pointer is invalid."
	case ErrDataOverlap:
		return "Input and output data arrays overlap."
	case ErrBadCallback:
		return "Invalid callback function provided."
	case ErrBadMode:
		return "Calling mode differs from initialization mode."
	case ErrNullCallback:
		return "Internal error: Callback function is nil during callback processing."
	case ErrNoVariableRatio:
		return "Converter does not support variable ratios."
	case ErrSincPrepareDataBadLen:
		return "Internal error: Bad length in Sinc prepare_data."
	case ErrBadInternalState:
		return "Internal error: Inconsistent state detected."
	default:
		return ""
	}
}

// mapGoErrorToCode attempts to convert a Go error back to an ErrorCode.
// This is imperfect but useful for StrError.
func mapGoErrorToCode(err error) ErrorCode {
	if err == nil {
		return ErrNoError
	}
	// This requires parsing the error string or using custom error types.
	// Simple approach for now: Check prefix? Not robust.
	// TODO: Implement robust mapping if needed, possibly using custom error types.
	// For now, return a generic code if mapping fails.
	var code ErrorCode
	if _, err := fmt.Sscanf(err.Error(), "libsamplerate error %d:", &code); err == nil {
		return code
	}
	return ErrBadInternalState // Default fallback
}

// --- Internal Dispatcher ---

// psrcSetConverter selects and initializes the specific converter state.
// Corresponds to static psrc_set_converter in samplerate.c (Updated)
// psrcSetConverter selects and initializes the specific converter state.
// Corresponds to static psrc_set_converter in samplerate.c (Updated)
func psrcSetConverter(converterType ConverterType, channels int) (*srcState, ErrorCode) {
	var state *srcState
	var errCode ErrorCode

	switch converterType {
	case SincBestQuality:
		if !enableSincBestConverter {
			return nil, ErrBadConverter
		}
		state, errCode = newSincState(converterType, channels)
	case SincMediumQuality:
		if !enableSincMediumConverter {
			return nil, ErrBadConverter
		}
		state, errCode = newSincState(converterType, channels)
	case SincFastest:
		if !enableSincFastConverter {
			return nil, ErrBadConverter
		}
		state, errCode = newSincState(converterType, channels)
	case ZeroOrderHold: // Added case
		state, errCode = newZohState(channels) // Use the new constructor
	case Linear:
		state, errCode = newLinearState(channels)
	default:
		return nil, ErrBadConverter
	}

	return state, errCode
}

// --- Utility Functions ---

// GetName returns the name of a converter type. (Updated)
func GetName(converterType ConverterType) string {
	if name := sincGetName(converterType); name != "" {
		return name
	}
	if name := zohGetNameInternal(converterType); name != "" {
		return name
	} // Use internal func
	if name := linearGetNameInternal(converterType); name != "" {
		return name
	}
	return "" // Unknown type
}

// GetDescription returns the description of a converter type. (Updated)
func GetDescription(converterType ConverterType) string {
	if desc := sincGetDescription(converterType); desc != "" {
		return desc
	}
	if desc := zohGetDescriptionInternal(converterType); desc != "" {
		return desc
	} // Use internal func
	if desc := linearGetDescriptionInternal(converterType); desc != "" {
		return desc
	}
	return "" // Unknown type
}

// --- Sample Format Conversion Helpers ---

// ShortToFloatArray converts a slice of int16 to float32.
func ShortToFloatArray(in []int16, out []float32) {
	count := minInt(len(in), len(out))
	scale := float32(1.0 / 32768.0) // 1.0 / 0x8000
	for i := 0; i < count; i++ {
		out[i] = float32(in[i]) * scale
	}
}

// FloatToShortArray converts a slice of float32 to int16 with clipping.
func FloatToShortArray(in []float32, out []int16) {
	count := minInt(len(in), len(out))
	scale := float64(32768.0)
	for i := 0; i < count; i++ {
		scaledValue := float64(in[i]) * scale
		// Use psfLrintf for rounding, consistent with C
		rounded := psfLrintf(float32(scaledValue)) // Round first

		// Clip
		if rounded >= 32767 {
			out[i] = 32767
		} else if rounded <= -32768 {
			out[i] = -32768
		} else {
			out[i] = int16(rounded)
		}
	}
}

// IntToFloatArray converts a slice of int32 to float32.
// (Revised for potentially better precision)
func IntToFloatArray(in []int32, out []float32) {
	count := minInt(len(in), len(out))
	// Use float64 for the scaling factor calculation for better precision
	scale64 := float64(1.0 / 2147483648.0) // 1.0 / 2^31
	for i := 0; i < count; i++ {
		// Perform calculation in float64 and convert to float32 at the end
		out[i] = float32(float64(in[i]) * scale64)
		// Alternative: Direct division might be optimized well?
		// out[i] = float32(float64(in[i]) / 2147483648.0)
	}
}

// FloatToIntArray converts a slice of float32 to int32 with clipping.
// (Ensure psfLrint is the updated version rounding half away from zero)
func FloatToIntArray(in []float32, out []int32) {
	count := minInt(len(in), len(out))
	scale := float64(2147483648.0)          // 2^31
	maxInt32Float := float64(math.MaxInt32) // 2147483647.0
	minInt32Float := float64(math.MinInt32) // -2147483648.0

	for i := 0; i < count; i++ {
		scaledValue := float64(in[i]) * scale

		// Clip BEFORE rounding - this might be important if scaling pushes value
		// slightly beyond representable range before rounding pulls it back.
		// C might rely on float->int conversion behavior for this.
		if scaledValue >= maxInt32Float+0.5 { // Check threshold slightly above max for rounding
			out[i] = math.MaxInt32
			continue
		}
		// For negative, check threshold slightly below min
		if scaledValue <= minInt32Float-0.5 {
			out[i] = math.MinInt32
			continue
		}

		// Now round using psfLrint (rounds half away from zero)
		rounded := psfLrint(scaledValue) // Returns int

		// Final check after rounding (paranoia, maybe not needed if above clips work)
		if rounded >= math.MaxInt32 {
			out[i] = math.MaxInt32
		} else if rounded <= math.MinInt32 {
			out[i] = math.MinInt32
		} else {
			out[i] = int32(rounded)
		}
	}
}

func sincGetName(converterType ConverterType) string {
	// TODO: Implement based on src_sinc.c:sinc_get_name
	switch converterType {
	case SincBestQuality:
		return "Best Sinc Interpolator"
	case SincMediumQuality:
		return "Medium Sinc Interpolator"
	case SincFastest:
		return "Fastest Sinc Interpolator"
	default:
		return ""
	}
}
func sincGetDescription(converterType ConverterType) string {
	// TODO: Implement based on src_sinc.c:sinc_get_description
	switch converterType {
	case SincBestQuality:
		return "Band limited sinc interpolation, best quality..."
	case SincMediumQuality:
		return "Band limited sinc interpolation, medium quality..."
	case SincFastest:
		return "Band limited sinc interpolation, fastest..."
	default:
		return ""
	}
}
func linearGetName(converterType ConverterType) string {
	// TODO: Implement based on src_linear.c:linear_get_name
	if converterType == Linear {
		return "Linear Interpolator"
	}
	return ""
}
func linearGetDescription(converterType ConverterType) string {
	// TODO: Implement based on src_linear.c:linear_get_description
	if converterType == Linear {
		return "Linear interpolation."
	}
	return ""
}

//func zohGetName(converterType ConverterType) string {
//	// TODO: Implement based on src_zoh.c:zoh_get_name
//	if converterType == ZeroOrderHold {
//		return "Zero Order Hold Interpolator"
//	}
//	return ""
//}
//func zohGetDescription(converterType ConverterType) string {
//	// TODO: Implement based on src_zoh.c:zoh_get_description
//	if converterType == ZeroOrderHold {
//		return "Zero order hold interpolation."
//	}
//	return ""
//}

// Update placeholder references if they existed
func zohGetName(converterType ConverterType) string { return zohGetNameInternal(converterType) } // Public wrapper if needed
func zohGetDescription(converterType ConverterType) string {
	return zohGetDescriptionInternal(converterType)
} // Public wrapper if needed
