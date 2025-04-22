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
	// "log"
)

// --- ZOH Specific Types ---

// zohFilter holds the private data for the ZOH converter.
// Corresponds to ZOH_DATA in src_zoh.c
type zohFilter struct {
	zohMagicMarker int       // Optional
	dirty          bool      // Flag to indicate if lastValue is initialized
	lastValue      []float32 // Stores the last input sample for each channel
}

const zohMagicMarker = 's' + ('r' << 4) + ('c' << 8) + ('z' << 12) + ('o' << 16) + ('h' << 20)

// --- ZOH State Management ---

// newZohFilterInternal creates the private data for the ZOH converter.
func newZohFilterInternal(channels int) (*zohFilter, error) {
	if channels <= 0 {
		return nil, fmt.Errorf("invalid channel count: %d", channels)
	}
	priv := &zohFilter{}
	priv.zohMagicMarker = zohMagicMarker
	priv.lastValue = make([]float32, channels)
	if priv.lastValue == nil {
		return nil, fmt.Errorf("failed to allocate lastValue buffer")
	}
	priv.dirty = false
	return priv, nil
}

// newZohState creates the main srcState for a ZOH converter.
func newZohState(channels int) (*srcState, ErrorCode) {
	if channels <= 0 {
		return nil, ErrBadChannelCount
	}

	state := &srcState{}
	state.channels = channels
	state.mode = ModeProcess

	filter, err := newZohFilterInternal(state.channels)
	if err != nil {
		return nil, ErrMallocFailed
	}
	state.privateData = filter
	state.vt = &zohStateVT

	resetErr := state.Reset() // Calls zohReset via VT method
	if resetErr != nil {
		return nil, mapGoErrorToCode(resetErr)
	}

	state.errCode = ErrNoError
	return state, ErrNoError
}

// zohReset resets the ZOH converter's private state.
func zohReset(state *srcState) {
	// ... (get filter, check ok) ...
	filter, ok := state.privateData.(*zohFilter)
	if !ok || filter == nil { /* handle error */
		return
	}

	// Reset state (actual reset logic)
	filter.dirty = false
	for i := range filter.lastValue {
		filter.lastValue[i] = 0.0
	}
}

// zohResetX resets the ZOH converter's private state.
func zohResetX(state *srcState) {
	if state == nil || state.privateData == nil {
		return
	}
	filter, ok := state.privateData.(*zohFilter)
	if !ok || filter == nil {
		return
	}

	filter.dirty = false
	for i := range filter.lastValue {
		filter.lastValue[i] = 0.0
	}
}

// zohClose handles cleanup for the ZOH converter.
func zohClose(state *srcState) {
	if state == nil || state.privateData == nil {
		return
	}
	_, ok := state.privateData.(*zohFilter)
	if !ok {
		return
	}
	state.privateData = nil // Allow GC
}

// zohCopy performs a deep copy of the ZOH converter state.
func zohCopy(state *srcState) *srcState {
	if state == nil || state.privateData == nil {
		return nil
	}
	origFilter, ok := state.privateData.(*zohFilter)
	if !ok || origFilter == nil {
		return nil
	}

	newState := &srcState{}
	*newState = *state
	newFilter := &zohFilter{}
	*newFilter = *origFilter

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

// --- ZOH Virtual Table ---

var zohStateVT = srcStateVT{
	variProcess:  zohVariProcess, // ZOH handles variable ratio directly
	constProcess: zohVariProcess, // Use the same function
	reset:        zohReset,
	copy:         zohCopy,
	close:        zohClose,
}

// --- ZOH Processing Function ---

// zohVariProcess performs zero-order hold "interpolation" (sample and hold).
// Corresponds to zoh_vari_process in src_zoh.c
func zohVariProcess(state *srcState, data *SrcData) ErrorCode {
	if data.InputFrames <= 0 {
		return ErrNoError
	}

	filter, ok := state.privateData.(*zohFilter)
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
	if srcRatio == 0 {
		return ErrBadSrcRatio
	} // Avoid division by zero

	channels := state.channels

	// --- Process samples using last_value before consuming first input frame ---
	// C: while (input_index < 1.0 && priv->out_gen < priv->out_count) { ... }
	for inputIndex < 1.0 && outGenSamples < outCountSamples {
		// C check: if (priv->in_used + state->channels * input_index >= priv->in_count) break ;
		// This seems less relevant for ZOH? We just need last_value.

		// Interpolate ratio if needed
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

		// Output the *last* value (from previous block or initial sample)
		outPos := int(outGenSamples)
		if outPos+channels > len(data.DataOut) {
			break
		} // Check output space

		copy(data.DataOut[outPos:outPos+channels], filter.lastValue) // Copy last held sample
		outGenSamples += int64(channels)

		// Figure out the next index.
		inputIndex += 1.0 / srcRatio
	}

	// --- Main Processing Loop ---
	initialFramesSkipped := int64(psfLrint(inputIndex - fmodOne(inputIndex)))
	inUsedSamples += initialFramesSkipped * int64(channels)
	inputIndex = fmodOne(inputIndex)

	// C: while (priv->out_gen < priv->out_count && priv->in_used + state->channels * input_index <= priv->in_count) { ... }
	// Loop while space in output AND the *previous* frame's data is available
	for outGenSamples < outCountSamples {
		// Determine index for the *previous* input frame (the one to hold)
		// This requires inUsedSamples >= channels
		y0BaseIndex := inUsedSamples - int64(channels)

		// Check if we have data for the frame *before* the one `inUsedSamples` points to.
		if y0BaseIndex < 0 {
			// Should only happen if initialFramesSkipped was 0, handled by first loop?
			break // Cannot access frame before the first one
		}
		// Also check if this required frame is within the current input block bounds
		if y0BaseIndex+int64(channels) > inCountSamples {
			// We need frame `y0`, but it's beyond the provided input.
			// This differs slightly from linear which needed `y1`. ZOH only needs `y0`.
			// Let's re-evaluate the C loop condition:
			// `priv->in_used + state->channels * input_index <= priv->in_count`
			// This seems to check if the *current* fractional position (`input_index`)
			// projected onto the input buffer (`priv->in_used + ...`) is within bounds.
			// This doesn't guarantee the *previous* frame needed (`y0BaseIndex`) is available.
			// Let's stick to the explicit check: Do we have the frame at `y0BaseIndex`?
			// If `y0BaseIndex` itself is valid (>=0), we can access it *if* it's
			// within the current `inputData` slice length (relative to start).
			// The effective length is `inCountSamples`.
			// So the condition is just `y0BaseIndex >= 0`. If it is, we use that frame.
			// The loop should terminate if `inUsedSamples` advances such that `y0BaseIndex`
			// would require data not yet available.
			// Let's adjust the loop condition to be simply `inUsedSamples >= int64(channels)` ?
			// No, the C loop runs while the *next read position* is valid.
			// Let's try the C condition more directly:
			// `current_read_pos = y0BaseIndex + inputIndex * channels` ??? Doesn't make sense.
			// Let's use: Loop while the *required previous frame* `y0BaseIndex` is available
			// within the current block `inCountSamples`.
			if y0BaseIndex+int64(channels) > inCountSamples {
				break // Required previous frame is beyond available input
			}
			// And also the check from linear: Is the *current* frame available?
			// This prevents reading past the end when calculating the *next* index advance.
			if inUsedSamples+int64(channels) > inCountSamples {
				break // Cannot calculate next step if current frame isn't fully available
			}

		}

		// Interpolate ratio if needed
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

		// Output the *previous* input sample (Zero-Order Hold)
		outPos := int(outGenSamples)
		if outPos+channels > len(data.DataOut) {
			break
		} // Check output space

		// Bounds check for input data access (y0)
		if y0BaseIndex < 0 || y0BaseIndex+int64(channels) > int64(len(inputData)) {
			return ErrBadInternalState // Logic error accessing y0
		}

		copy(data.DataOut[outPos:outPos+channels], inputData[y0BaseIndex:y0BaseIndex+int64(channels)])
		outGenSamples += int64(channels)

		// Figure out the next index.
		inputIndex += 1.0 / srcRatio
		intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex))
		inUsedSamples += int64(intInputAdvance) * int64(channels)
		inputIndex = fmodOne(inputIndex)
	}

	// --- Final State Update ---
	if inUsedSamples > inCountSamples {
		overshotFrames := (inUsedSamples - inCountSamples) / int64(channels)
		inputIndex += float64(overshotFrames)
		inUsedSamples = inCountSamples
	}

	state.lastPosition = inputIndex

	// Update last_value with the last fully consumed input sample frame
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

	data.InputFramesUsed = inUsedSamples / int64(channels)
	data.OutputFramesGen = outGenSamples / int64(channels)

	return ErrNoError
}

// --- Name/Description ---
func zohGetNameInternal(srcEnum ConverterType) string {
	if srcEnum == ZeroOrderHold {
		return "ZOH Interpolator"
	}
	return ""
}

func zohGetDescriptionInternal(srcEnum ConverterType) string {
	if srcEnum == ZeroOrderHold {
		return "Zero order hold interpolator, very fast, poor quality."
	}
	return ""
}
