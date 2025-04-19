// linear.go
package libsamplerate

import (
	"fmt"
	"math"
	// "log"
)

// --- Linear Specific Types ---

// linearFilter holds the private data for the linear converter.
// Corresponds to LINEAR_DATA in src_linear.c
type linearFilter struct {
	linearMagicMarker int       // Optional: for internal consistency checks
	dirty             bool      // Flag to indicate if lastValue is initialized
	lastValue         []float32 // Stores the last input sample for each channel
}

const linearMagicMarker = 'l' + ('i' << 4) + ('n' << 8) + ('e' << 12) + ('a' << 16) + ('r' << 20)

// --- Linear State Management ---

// newLinearFilterInternal creates the private data for the linear converter.
func newLinearFilterInternal(channels int) (*linearFilter, error) {
	if channels <= 0 {
		return nil, fmt.Errorf("invalid channel count: %d", channels)
	}
	priv := &linearFilter{}
	priv.linearMagicMarker = linearMagicMarker
	priv.lastValue = make([]float32, channels) // Allocates and zero-initializes
	if priv.lastValue == nil {
		// 'make' typically panics on failure, but defensive check
		return nil, fmt.Errorf("failed to allocate lastValue buffer")
	}
	priv.dirty = false // Start clean
	return priv, nil
}

// newLinearState creates the main srcState for a Linear converter.
func newLinearState(channels int) (*srcState, ErrorCode) {
	if channels <= 0 {
		return nil, ErrBadChannelCount
	}

	state := &srcState{}
	state.channels = channels
	state.mode = ModeProcess

	filter, err := newLinearFilterInternal(state.channels)
	if err != nil {
		return nil, ErrMallocFailed
	}
	state.privateData = filter
	state.vt = &linearStateVT

	// ** Correction: Check error from Reset **
	resetErr := state.Reset() // Calls linearReset via VT method
	if resetErr != nil {
		// If reset fails, the state is inconsistent.
		// We don't have much to clean up beyond GC in pure Go,
		// but conceptually, return the error.
		// Map Go error back to ErrorCode if possible.
		return nil, mapGoErrorToCode(resetErr)
	}

	state.errCode = ErrNoError
	return state, ErrNoError
}

// linearReset resets the linear converter's private state.
func linearReset(state *srcState) {
	if state == nil || state.privateData == nil {
		return
	}
	filter, ok := state.privateData.(*linearFilter)
	if !ok || filter == nil {
		return
	} // Invalid type or nil

	filter.dirty = false
	// Zero out lastValue slice
	for i := range filter.lastValue {
		filter.lastValue[i] = 0.0
	}
}

// linearClose handles cleanup for the linear converter.
func linearClose(state *srcState) {
	if state == nil || state.privateData == nil {
		return
	}
	_, ok := state.privateData.(*linearFilter)
	if !ok {
		return
	}

	// Only need to nil out reference for GC; slice buffer is managed by GC.
	state.privateData = nil
}

// linearCopy performs a deep copy of the linear converter state.
func linearCopy(state *srcState) *srcState {
	if state == nil || state.privateData == nil {
		return nil
	}
	origFilter, ok := state.privateData.(*linearFilter)
	if !ok || origFilter == nil {
		return nil
	}

	// Shallow copy state, create new filter
	newState := &srcState{}
	*newState = *state
	newFilter := &linearFilter{}
	*newFilter = *origFilter // Copy magic, dirty flag

	// Deep copy lastValue slice
	if len(origFilter.lastValue) > 0 {
		newFilter.lastValue = make([]float32, len(origFilter.lastValue))
		copy(newFilter.lastValue, origFilter.lastValue)
	} else {
		newFilter.lastValue = nil
	}

	newState.privateData = newFilter
	newState.errCode = ErrNoError
	return newState
}

// --- Linear Virtual Table ---

var linearStateVT = srcStateVT{
	variProcess:  linearVariProcess, // Linear handles variable ratio directly
	constProcess: linearVariProcess, // Use the same function for constant ratio
	reset:        linearReset,
	copy:         linearCopy,
	close:        linearClose,
}

// --- Linear Processing Function ---

// linearVariProcess performs linear interpolation.
// Corresponds to linear_vari_process in src_linear.c (Corrected version)
func linearVariProcess(state *srcState, data *SrcData) ErrorCode {
	if data.InputFrames <= 0 {
		return ErrNoError
	}

	filter, ok := state.privateData.(*linearFilter)
	if !ok || filter == nil {
		return ErrBadState
	}

	inputIndex := state.lastPosition
	srcRatio := state.lastRatio

	inCountSamples := data.InputFrames * int64(state.channels)
	outCountSamples := data.OutputFrames * int64(state.channels)
	data.InputFramesUsed = 0
	data.OutputFramesGen = 0
	var inUsedSamples int64 = 0
	var outGenSamples int64 = 0

	if len(data.DataIn) == 0 {
		return ErrBadDataPtr
	}
	inputData := data.DataIn

	if !filter.dirty {
		if inCountSamples >= int64(state.channels) {
			copy(filter.lastValue, inputData[:state.channels])
			filter.dirty = true
		} else {
			return ErrBadData
		}
	}

	if isBadSrcRatio(srcRatio) {
		if isBadSrcRatio(data.SrcRatio) {
			return ErrBadSrcRatio
		}
		srcRatio = data.SrcRatio
		state.lastRatio = srcRatio
	}

	channels := state.channels

	// --- Process samples using last_value and the first input sample ---
	for inputIndex < 1.0 && outGenSamples < outCountSamples {
		if outCountSamples > 0 && math.Abs(state.lastRatio-data.SrcRatio) > srcMinRatioDiff {
			srcRatio = state.lastRatio + float64(outGenSamples)*(data.SrcRatio-state.lastRatio)/float64(outCountSamples)
			if isBadSrcRatio(srcRatio) { // Clamp
				if srcRatio < 1.0/srcMaxRatio {
					srcRatio = 1.0 / srcMaxRatio
				}
				if srcRatio > srcMaxRatio {
					srcRatio = srcMaxRatio
				}
			}
		}
		if srcRatio == 0 {
			return ErrBadSrcRatio
		}

		outPos := int(outGenSamples)
		if outPos+channels > len(data.DataOut) {
			break
		}

		for ch := 0; ch < channels; ch++ {
			lastVal := float64(filter.lastValue[ch])
			firstVal := float64(inputData[ch])
			data.DataOut[outPos+ch] = float32(lastVal + inputIndex*(firstVal-lastVal))
		}
		outGenSamples += int64(channels)
		inputIndex += 1.0 / srcRatio
	}

	// --- Main Processing Loop ---
	initialFramesSkipped := int64(psfLrint(inputIndex - fmodOne(inputIndex)))
	inUsedSamples += initialFramesSkipped * int64(channels)
	inputIndex = fmodOne(inputIndex)

	for outGenSamples < outCountSamples {
		y1BaseIndex := inUsedSamples
		if y1BaseIndex+int64(channels) > inCountSamples {
			break
		}
		if y1BaseIndex < int64(channels) {
			break
		} // Should be handled by first loop
		y0BaseIndex := y1BaseIndex - int64(channels)

		if outCountSamples > 0 && math.Abs(state.lastRatio-data.SrcRatio) > srcMinRatioDiff {
			srcRatio = state.lastRatio + float64(outGenSamples)*(data.SrcRatio-state.lastRatio)/float64(outCountSamples)
			if isBadSrcRatio(srcRatio) { // Clamp
				if srcRatio < 1.0/srcMaxRatio {
					srcRatio = 1.0 / srcMaxRatio
				}
				if srcRatio > srcMaxRatio {
					srcRatio = srcMaxRatio
				}
			}
		}
		if srcRatio == 0 {
			return ErrBadSrcRatio
		}

		outPos := int(outGenSamples)
		if outPos+channels > len(data.DataOut) {
			break
		}

		if y0BaseIndex < 0 || y1BaseIndex+int64(channels) > int64(len(inputData)) {
			return ErrBadInternalState
		}

		for ch := 0; ch < channels; ch++ {
			y0 := float64(inputData[y0BaseIndex+int64(ch)])
			y1 := float64(inputData[y1BaseIndex+int64(ch)])
			data.DataOut[outPos+ch] = float32(y0 + inputIndex*(y1-y0))
		}
		outGenSamples += int64(channels)

		// Figure out the next index.
		inputIndex += 1.0 / srcRatio
		// **Correction applied here:** Use := to declare intInputAdvance in this scope
		intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex)) // <-- FIX: Use :=
		inUsedSamples += int64(intInputAdvance) * int64(channels)     // Advance input usage marker (use int64 conversion)
		inputIndex = fmodOne(inputIndex)                              // Keep fractional part
	}

	// --- Final State Update ---
	if inUsedSamples > inCountSamples {
		overshotFrames := (inUsedSamples - inCountSamples) / int64(channels)
		inputIndex += float64(overshotFrames)
		inUsedSamples = inCountSamples
	}

	state.lastPosition = inputIndex

	if inUsedSamples >= int64(channels) {
		lastFrameOffset := inUsedSamples - int64(channels)
		if lastFrameOffset+int64(channels) <= int64(len(inputData)) {
			copy(filter.lastValue, inputData[lastFrameOffset:lastFrameOffset+int64(channels)])
			filter.dirty = true
		} else {
			return ErrBadInternalState
		}
	} else if !filter.dirty && inCountSamples >= int64(channels) {
		copy(filter.lastValue, inputData[:channels])
		filter.dirty = true
	}

	state.lastRatio = srcRatio

	data.InputFramesUsed = inUsedSamples / int64(channels) // Use InputFramesUsed (corrected typo)
	data.OutputFramesGen = outGenSamples / int64(channels) // Use OutputFramesGen (corrected typo)

	return ErrNoError
}

// --- Name/Description ---
// (Implement placeholders in samplerate.go based on these)

func linearGetNameInternal(srcEnum ConverterType) string { // Internal name
	if srcEnum == Linear {
		return "Linear Interpolator"
	}
	return ""
}

func linearGetDescriptionInternal(srcEnum ConverterType) string { // Internal name
	if srcEnum == Linear {
		return "Linear interpolator, very fast, poor quality."
	}
	return ""
}
