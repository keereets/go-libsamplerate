// sinc.go
package libsamplerate

import (
	"fmt"
	"math"
)

// --- Sinc Specific Types ---

// sincFilter holds the private data for a sinc converter instance.
// Corresponds to SINC_FILTER in src_sinc.c
type sincFilter struct {
	sincMagicMarker int // Optional: for internal consistency checks

	// These seem unused based on src_sinc.c, maybe used elsewhere?
	// Keep them for now, matching C struct.
	// inCount int64 // Input samples available (set by process func) - redundant w/ data.InputFrames?
	// inUsed  int64 // Input samples consumed (set by process func) - redundant w/ data.InputFramesUsed?
	// outCount int64 // Output samples requested (set by process func) - redundant w/ data.OutputFrames?
	// outGen  int64 // Output samples generated (set by process func) - redundant w/ data.OutputFramesGen?

	coeffHalfLen int // Number of coefficients on one side of center (len(Coeffs) - 2)
	indexInc     int // Fixed point increment per unit index (from coeffData)

	// Note: src_ratio and input_index are stored in srcState (lastRatio, lastPosition)
	// The C struct duplicated them; avoid duplication in Go version?
	// Let's omit them here and use state.lastRatio / state.lastPosition.

	coeffs []float32 // Coefficient table (slice referencing global data)

	bCurrent int // Current read position in buffer (index)
	bEnd     int // Current write position (end of valid data) in buffer (index)
	bRealEnd int // Marker for real end of data when input ends (index, or -1)
	bLen     int // Total allocated length of the buffer slice

	// Pre-allocated temporary calculation buffers (avoids allocation during processing)
	leftCalc  [maxChannels]float64 // Matched C array size
	rightCalc [maxChannels]float64

	buffer []float32 // Main internal processing buffer (ring buffer)
}

// Fixed-point math constants and types specific to Sinc
const (
	shiftBits = 12
	fpOne     = 1 << shiftBits // Fixed point representation of 1.0
	invFpOne  = 1.0 / float64(fpOne)
)

// incrementT is the fixed-point type used for filter calculations
type incrementT = int32 // Match C typedef int32_t increment_t

// --- Fixed-Point Helpers ---

// doubleToFP converts float64 to fixed-point incrementT.
func doubleToFP(x float64) incrementT {
	return incrementT(psfLrint(x * float64(fpOne)))
}

// intToFP converts int to fixed-point incrementT.
func intToFP(x int) incrementT {
	// Check for potential overflow before shifting
	if x > math.MaxInt32>>shiftBits || x < math.MinInt32>>shiftBits {
		// This indicates an internal logic error if coeffHalfLen is too large
		panic(fmt.Sprintf("intToFP overflow: %d", x))
	}
	return incrementT(x << shiftBits)
}

// fpToInt converts fixed-point incrementT back to int (integer part).
func fpToInt(x incrementT) int {
	return int(x >> shiftBits)
}

// fpFractionPart extracts the fractional part of a fixed-point incrementT.
func fpFractionPart(x incrementT) incrementT {
	return x & (fpOne - 1) // Mask out integer part
}

// fpToDouble converts the fractional part of fixed-point incrementT to float64.
func fpToDouble(x incrementT) float64 {
	// Only consider the fractional part for interpolation
	return float64(fpFractionPart(x)) * invFpOne
}

// --- Integer Division Ceiling ---
// Calculates ceil(a / b) for positive integers
func intDivCeil(a, b int) int {
	if a < 0 || b <= 0 {
		// Match C assert behavior - should not happen in this context
		panic(fmt.Sprintf("intDivCeil precondition violation: a=%d, b=%d", a, b))
	}
	return (a + (b - 1)) / b
}

// --- Sinc State Management ---

// newSincFilterInternal creates and initializes the sincFilter private data structure.
// Corresponds to sinc_filter_new in src_sinc.c
func newSincFilterInternal(converterType ConverterType, channels int) (*sincFilter, error) {
	// Validation already done in newSincState, but double check
	if channels <= 0 || channels > maxChannels {
		return nil, fmt.Errorf("invalid channel count: %d (must be 1-%d)", channels, maxChannels)
	}

	priv := &sincFilter{}
	// priv.sincMagicMarker = SINC_MAGIC_MARKER // Optional

	// Select Coefficients
	var coeffSource coeffData
	var ok bool
	switch converterType {
	case SincFastest:
		if !enableSincFastConverter {
			return nil, fmt.Errorf("SincFastest converter not enabled")
		}
		coeffSource = fastestCoeffs
		ok = len(fastestCoeffs.Coeffs) > 0 && fastestCoeffs.Increment > 0
	case SincMediumQuality:
		if !enableSincMediumConverter {
			return nil, fmt.Errorf("SincMediumQuality converter not enabled")
		}
		coeffSource = midQualCoeffs
		ok = len(midQualCoeffs.Coeffs) > 0 && midQualCoeffs.Increment > 0
	case SincBestQuality:
		if !enableSincBestConverter {
			return nil, fmt.Errorf("SincBestQuality converter not enabled")
		}
		coeffSource = highQualCoeffs
		ok = len(highQualCoeffs.Coeffs) > 0 && highQualCoeffs.Increment > 0
	default:
		return nil, fmt.Errorf("internal error: unexpected sinc converter type %d", converterType)
	}

	if !ok {
		// This indicates the coefficient data wasn't loaded in coeffs.go
		return nil, fmt.Errorf("coefficient data for converter type %d not loaded or invalid", converterType)
	}

	priv.coeffs = coeffSource.Coeffs
	priv.coeffHalfLen = len(priv.coeffs) - 2
	priv.indexInc = coeffSource.Increment

	if priv.coeffHalfLen < 0 { // Should have at least 2 coeffs (e.g., value and 0.0 end)
		return nil, fmt.Errorf("invalid coefficient length %d for converter %d", len(priv.coeffs), converterType)
	}

	// Calculate Buffer Length (bLen)
	calcLen := 3 * psfLrint((float64(priv.coeffHalfLen)+2.0)/float64(priv.indexInc)*srcMaxRatio+1.0)
	priv.bLen = maxInt(calcLen, 4096)
	priv.bLen *= channels
	priv.bLen += 1 // For C's <= check against samples_in_hand

	// Allocate Buffer
	bufferSize := priv.bLen + channels // C allocates extra for sanity check area
	if bufferSize <= 0 {
		return nil, fmt.Errorf("calculated negative or zero buffer size: %d", bufferSize)
	}
	priv.buffer = make([]float32, bufferSize)

	// C returns NULL if buffer allocation fails. Go's 'make' panics. Assume success.
	priv.bRealEnd = -1 // Initialize

	return priv, nil
}

// newSincState creates the main srcState for a Sinc converter.
func newSincState(converterType ConverterType, channels int) (*srcState, ErrorCode) {
	// Basic validation
	switch converterType {
	case SincFastest, SincMediumQuality, SincBestQuality: // OK
	default:
		return nil, ErrBadConverter
	}
	if channels <= 0 {
		return nil, ErrBadChannelCount
	}
	if channels > maxChannels {
		return nil, ErrBadChannelCount
	}

	state := &srcState{}
	state.channels = channels
	state.mode = ModeProcess

	filter, err := newSincFilterInternal(converterType, channels)
	if err != nil {
		// fmt.Printf("Error creating sinc filter: %v\n", err) // Debug
		return nil, ErrMallocFailed
	}
	state.privateData = filter

	switch channels { // Assign VT based on channels
	case 1:
		state.vt = &sincMonoStateVT
	case 2:
		state.vt = &sincStereoStateVT
	case 4:
		state.vt = &sincQuadStateVT
	case 6:
		state.vt = &sincHexStateVT
	default:
		state.vt = &sincMultichanStateVT
	}

	// ** Correction: Check error from Reset **
	resetErr := state.Reset() // Calls sincReset via VT method
	if resetErr != nil {
		// Need to clean up allocated filter if reset fails?
		// In Go, GC handles 'filter' if 'state' isn't returned,
		// but calling Close might be conceptually cleaner if it did resource cleanup.
		// state.Close() // Call Close to potentially release resources held by filter?
		return nil, mapGoErrorToCode(resetErr)
	}

	state.errCode = ErrNoError
	return state, ErrNoError
}

// sincReset resets the internal state of the Sinc filter.
func sincReset(state *srcState) {
	if state == nil || state.privateData == nil {
		return
	}
	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		fmt.Printf("Error/Warning: sincReset: PrivateData is not a valid *sincFilter (type: %T)\n", state.privateData)
		return
	}

	// Reset buffer pointers and state
	filter.bCurrent = 0
	filter.bEnd = 0
	filter.bRealEnd = -1

	// Don't reset state.lastRatio/lastPosition here, C src_reset handles common fields

	// Zero out the main part of the buffer
	if filter.bLen > 0 && len(filter.buffer) > 0 {
		zeroLen := minInt(filter.bLen, len(filter.buffer)) // Ensure bounds
		bufferToZero := filter.buffer[:zeroLen]
		for i := range bufferToZero {
			bufferToZero[i] = 0.0
		}
	}

	// Set the sanity check area after the main buffer data
	sanityCheckValue := float32(170.0) // 0xAA
	start := filter.bLen
	count := state.channels
	end := start + count

	if len(filter.buffer) > 0 && count > 0 && start >= 0 {
		if end > len(filter.buffer) {
			end = len(filter.buffer) // Clip to actual buffer size
		}
		if end > start {
			sanitySlice := filter.buffer[start:end]
			for i := range sanitySlice {
				sanitySlice[i] = sanityCheckValue
			}
		}
	}
}

// sincClose handles any Sinc-specific cleanup.
func sincClose(state *srcState) {
	if state == nil || state.privateData == nil {
		return
	}
	_, ok := state.privateData.(*sincFilter)
	if !ok {
		// Already closed or invalid state?
		return
	}
	// In pure Go with GC, we mainly just need to nil out references
	// to help the GC and prevent accidental reuse via stale pointers.
	// The buffer slice will be GC'd.
	state.privateData = nil // Allow filter and its buffer to be GC'd
	// The main Close method in samplerate.go already nils state.vt etc.
}

// sincCopy performs a deep copy of the Sinc converter state.
func sincCopy(state *srcState) *srcState {
	if state == nil || state.privateData == nil {
		// Cannot copy nil state
		return nil // C returns NULL
	}
	origFilter, ok := state.privateData.(*sincFilter)
	if !ok || origFilter == nil {
		// Invalid state to copy
		state.errCode = ErrBadState // Mark original state as bad? C doesn't seem to.
		return nil
	}

	// 1. Create new outer state and copy simple fields
	newState := &srcState{}
	*newState = *state // Shallow copy of state fields first (vt, ratios, mode, etc.)

	// 2. Create new filter state and copy simple fields
	newFilter := &sincFilter{}
	*newFilter = *origFilter // Shallow copy filter fields (magic, lens, incs, pointers)

	// 3. Deep copy the buffer
	if len(origFilter.buffer) > 0 {
		newFilter.buffer = make([]float32, len(origFilter.buffer))
		copy(newFilter.buffer, origFilter.buffer)
	} else {
		newFilter.buffer = nil // Ensure it's nil if original was
	}

	// 4. Coeffs slice can be shared (points to global data)
	// newFilter.coeffs = origFilter.coeffs // Already copied by struct copy

	// 5. Copy scratch arrays (leftCalc, rightCalc are arrays, copied by struct copy)

	// 6. Assign new filter to new state's privateData
	newState.privateData = newFilter

	// 7. VT pointer is already copied by struct copy

	newState.errCode = ErrNoError // Ensure new state starts without error
	return newState
}

// --- Sinc Virtual Table Definitions ---

var sincMonoStateVT = srcStateVT{
	variProcess:  sincMonoVariProcess,
	constProcess: sincMonoVariProcess, // C uses same func for const/vari
	reset:        sincReset,
	copy:         sincCopy,
	close:        sincClose,
}

var sincStereoStateVT = srcStateVT{
	variProcess:  sincStereoVariProcess,
	constProcess: sincStereoVariProcess,
	reset:        sincReset,
	copy:         sincCopy,
	close:        sincClose,
}

var sincQuadStateVT = srcStateVT{
	variProcess:  sincQuadVariProcess,
	constProcess: sincQuadVariProcess,
	reset:        sincReset,
	copy:         sincCopy,
	close:        sincClose,
}

var sincHexStateVT = srcStateVT{
	variProcess:  sincHexVariProcess,
	constProcess: sincHexVariProcess,
	reset:        sincReset,
	copy:         sincCopy,
	close:        sincClose,
}

var sincMultichanStateVT = srcStateVT{
	variProcess:  sincMultichanVariProcess,
	constProcess: sincMultichanVariProcess,
	reset:        sincReset,
	copy:         sincCopy,
	close:        sincClose,
}

// --- TODO: Implement Processing & Helper Functions ---
// sincMonoVariProcess, sincStereoVariProcess, etc.
// calcOutputSingle, calcOutputStereo, etc.
// prepareData

// prepareData manages the internal buffer, loading new data as needed.
// Corresponds to prepare_data in src_sinc.c
func prepareData(filter *sincFilter, channels int, data *SrcData, halfFilterChanLen int) ErrorCode {
	// Ensure valid input indices from SrcData
	// C uses filter->in_count, filter->in_used directly, but we rely on SrcData fields
	inCount := data.InputFrames * int64(channels)
	inUsed := data.InputFramesUsed * int64(channels)

	// C: if (filter->b_real_end >= 0) return SRC_ERR_NO_ERROR;
	if filter.bRealEnd >= 0 {
		return ErrNoError // Already marked end-of-input, don't load more.
	}

	// C: if (data->data_in == NULL) return SRC_ERR_NO_ERROR;
	// In Go, check slice length. If InputFrames > 0, DataIn must be valid.
	if data.InputFrames > 0 && len(data.DataIn) == 0 {
		// This condition should have been caught by Process, but double-check
		return ErrBadDataPtr
	}
	// If no new input frames are provided, we also don't need to load.
	if data.InputFrames == 0 {
		// If it's also end_of_input, we might need padding later.
		// But for loading, there's nothing to do now.
		// However, the C code proceeds to calculate 'len' based on buffer state.
		// Let's stick closer to C logic flow. If data_in is conceptually NULL,
		// but input_frames is > 0, that's an error. If input_frames is 0,
		// there's just no data *to* copy, but we might still wrap the buffer.
		// The C check `data->data_in == NULL` seems to imply "no buffer provided".
		// Let's refine: if no *more* frames available, return.
		if inUsed >= inCount && !data.EndOfInput {
			// No more frames available in this data block, and not EOF yet.
			return ErrNoError // Nothing to load right now.
		}
		// If inUsed >= inCount AND data.EndOfInput is true, we fall through to padding logic below.
	}

	currentDataOffset := int(inUsed) // Start offset in data.DataIn slice

	var requiredLen int // How many samples we need to load

	// Buffer management logic:
	if filter.bCurrent == 0 {
		// Initial state or after wrap-around where bCurrent is reset to 0 implicitly by % op.
		// However, C code uses b_current==0 specifically for *initial* state setup.
		// This might need refinement if buffer wrap makes b_current exactly 0 later.
		// Assuming C's intent: First time filling the buffer.

		// C: len = filter->b_len - 2 * half_filter_chan_len ;
		requiredLen = filter.bLen - (2 * halfFilterChanLen)
		if requiredLen < 0 {
			requiredLen = 0
		} // Ensure non-negative

		// C: filter->b_current = filter->b_end = half_filter_chan_len ;
		// Set initial read/write pointers after the initial zero-padding area.
		filter.bCurrent = halfFilterChanLen
		filter.bEnd = halfFilterChanLen
		// Buffer from 0 to halfFilterChanLen-1 is implicitly zero (from make or reset)

	} else if filter.bEnd+halfFilterChanLen+channels < filter.bLen {
		// Enough space at the end of the buffer to load new data directly.
		// C: len = MAX (filter->b_len - filter->b_current - half_filter_chan_len, 0) ;
		// This seems wrong in C? Should be based on b_end? Let's recalculate based on available space.
		// Available space at end = filter.bLen - filter.bEnd
		// Let's rethink requiredLen: how much *can* we load?
		availableSpaceAtEnd := filter.bLen - filter.bEnd
		requiredLen = availableSpaceAtEnd // Max we can load without wrapping

	} else {
		// Need to wrap data from the end back to the beginning.
		// C: len = filter->b_end - filter->b_current ; (length of valid data currently)
		validDataLen := filter.bEnd - filter.bCurrent
		if validDataLen < 0 {
			// This implies b_end < b_current, which shouldn't happen with append-only logic?
			// If it's a ring buffer where b_end wrapped but b_current hasn't, this calculation is wrong.
			// Let's assume C uses simple append and wrap: b_end is always >= b_current before wrap.
			return ErrBadInternalState // Should not happen
		}

		// C: memmove (filter->buffer, filter->buffer + filter->b_current - half_filter_chan_len,
		//             (half_filter_chan_len + len) * sizeof (filter->buffer [0])) ;
		// Go: copy(dest, src)
		srcStart := filter.bCurrent - halfFilterChanLen // Start of data to preserve (incl lookback)
		copyLen := halfFilterChanLen + validDataLen

		// Bounds checks for source slice
		if srcStart < 0 {
			return ErrBadInternalState
		} // Should have enough lookback space
		if srcStart+copyLen > len(filter.buffer) {
			fmt.Printf("prepareData wrap src bounds error: srcStart=%d, copyLen=%d, bufLen=%d\n", srcStart, copyLen, len(filter.buffer))
			return ErrBadInternalState // Trying to copy past end of allocated buffer
		}

		// Check destination bounds (copying to start of buffer)
		if copyLen > len(filter.buffer) {
			fmt.Printf("prepareData wrap dest bounds error: copyLen=%d, bufLen=%d\n", copyLen, len(filter.buffer))
			return ErrBadInternalState // Cannot copy more than buffer size
		}

		// Perform the copy using Go's copy function (handles overlap)
		copy(filter.buffer[0:copyLen], filter.buffer[srcStart:srcStart+copyLen])

		// C: filter->b_current = half_filter_chan_len ;
		// C: filter->b_end = filter->b_current + len ;
		filter.bCurrent = halfFilterChanLen          // New read position is after preserved lookback
		filter.bEnd = filter.bCurrent + validDataLen // New write position is after copied valid data

		// Now calculate how much space is available to load new data after the wrap
		// C: len = MAX (filter->b_len - filter->b_current - half_filter_chan_len, 0) ; (C code uses len ambiguously)
		// Let's use a clearer name: spaceToLoad
		// Should be space from new b_end to end of usable buffer
		// Usable buffer length seems to be bLen according to C calculations?
		spaceToLoad := filter.bLen - filter.bEnd
		requiredLen = spaceToLoad // Max we can load now
	}

	// Now, determine how much data to actually copy from input
	// C: len = MIN ((int) (filter->in_count - filter->in_used), len) ;
	framesAvailable := inCount - inUsed
	if framesAvailable < 0 {
		framesAvailable = 0
	}

	copyCount := minInt(int(framesAvailable), requiredLen) // Samples to copy

	// C: len -= (len % channels) ; // Ensure whole frames
	copyCount -= (copyCount % channels)

	// C: if (len < 0 || filter->b_end + len > filter->b_len) return SRC_ERR_SINC_PREPARE_DATA_BAD_LEN;
	if copyCount < 0 {
		return ErrSincPrepareDataBadLen // Cannot copy negative amount
	}
	// Check if copy destination is valid
	if filter.bEnd+copyCount > filter.bLen {
		// This check seems redundant if requiredLen was calculated correctly based on bLen.
		// C uses filter.b_len for both usable length and allocated size? Let's stick to bLen.
		fmt.Printf("prepareData copy bounds error: bEnd=%d, copyCount=%d, bLen=%d\n", filter.bEnd, copyCount, filter.bLen)
		return ErrSincPrepareDataBadLen
	}
	// Also check against the actual allocated buffer length
	if filter.bEnd+copyCount > len(filter.buffer) {
		fmt.Printf("prepareData copy alloc bounds error: bEnd=%d, copyCount=%d, allocLen=%d\n", filter.bEnd, copyCount, len(filter.buffer))
		return ErrSincPrepareDataBadLen // Cannot write past allocated buffer
	}

	// Perform the copy from input data to internal buffer if there's data to copy
	if copyCount > 0 {
		// C: memcpy (filter->buffer + filter->b_end, data->data_in + filter->in_used, len * sizeof (filter->buffer [0])) ;
		// Ensure source slice bounds are okay
		if currentDataOffset+copyCount > len(data.DataIn) {
			return ErrBadData // Trying to read past end of provided input slice
		}

		copy(filter.buffer[filter.bEnd:filter.bEnd+copyCount], data.DataIn[currentDataOffset:currentDataOffset+copyCount])

		// C: filter->b_end += len ;
		filter.bEnd += copyCount
		// C: filter->in_used += len ;
		inUsed += int64(copyCount) // Update local count
	}

	// Update SrcData with consumed frames (convert samples back to frames)
	data.InputFramesUsed = inUsed / int64(channels)

	// Handle End Of Input: Add zero padding if needed.
	// C: if (filter->in_used == filter->in_count &&
	//         filter->b_end - filter->b_current < 2 * half_filter_chan_len && data->end_of_input)
	// This condition seems complex. Let's break it down:
	// 1. Have we consumed all input provided *in this block*? (inUsed >= inCount)
	// 2. Is this *actually* the end of all input? (data.EndOfInput is true)
	// 3. Do we have less data in the buffer than needed for lookahead/lookback? (filter.bEnd - filter.bCurrent < 2 * halfFilterChanLen)
	// If all true, we need to pad.
	inputFullyConsumed := (inUsed >= inCount)
	hasEnoughLookaround := (filter.bEnd - filter.bCurrent) >= (2 * halfFilterChanLen)

	if inputFullyConsumed && data.EndOfInput && !hasEnoughLookaround {
		// Need to pad with zeros

		// C first checks if buffer needs wrapping *again* before padding
		// C: if (filter->b_len - filter->b_end < half_filter_chan_len + 5) ... memmove ...
		requiredPaddingSpace := halfFilterChanLen + 5 // C uses +5 margin?
		if filter.bLen-filter.bEnd < requiredPaddingSpace {
			// Need to wrap existing data to make space for padding at the end

			validDataLen := filter.bEnd - filter.bCurrent
			if validDataLen < 0 {
				return ErrBadInternalState
			}

			srcStart := filter.bCurrent - halfFilterChanLen
			copyLen := halfFilterChanLen + validDataLen

			// Bounds checks (similar to above wrap logic)
			if srcStart < 0 {
				return ErrBadInternalState
			}
			if srcStart+copyLen > len(filter.buffer) {
				fmt.Printf("prepareData pad-wrap src bounds: srcStart=%d, copyLen=%d, bufLen=%d\n", srcStart, copyLen, len(filter.buffer))
				return ErrBadInternalState
			}
			if copyLen > len(filter.buffer) {
				fmt.Printf("prepareData pad-wrap dest bounds: copyLen=%d, bufLen=%d\n", copyLen, len(filter.buffer))
				return ErrBadInternalState
			}

			copy(filter.buffer[0:copyLen], filter.buffer[srcStart:srcStart+copyLen])

			filter.bCurrent = halfFilterChanLen
			filter.bEnd = filter.bCurrent + validDataLen
		}

		// Now add zero padding
		// C: filter->b_real_end = filter->b_end ;
		filter.bRealEnd = filter.bEnd // Mark the end of actual data

		// C: len = half_filter_chan_len + 5 ; (Padding amount)
		paddingLen := halfFilterChanLen + 5

		// C: if (len < 0 || filter->b_end + len > filter->b_len) len = filter->b_len - filter->b_end ;
		// Ensure padding doesn't exceed usable buffer length (bLen)
		if paddingLen < 0 {
			paddingLen = 0
		} // Should not happen
		if filter.bEnd+paddingLen > filter.bLen {
			paddingLen = filter.bLen - filter.bEnd
		}
		// Also ensure padding doesn't exceed allocated length
		if filter.bEnd+paddingLen > len(filter.buffer) {
			paddingLen = len(filter.buffer) - filter.bEnd
		}

		// C: memset (filter->buffer + filter->b_end, 0, len * sizeof (filter->buffer [0])) ;
		if paddingLen > 0 {
			paddingSlice := filter.buffer[filter.bEnd : filter.bEnd+paddingLen]
			for i := range paddingSlice {
				paddingSlice[i] = 0.0
			}
			// C: filter->b_end += len ;
			filter.bEnd += paddingLen
		}
	}

	return ErrNoError
}

// calcOutputSingle calculates a single interpolated output sample.
// Corresponds to calc_output_single in src_sinc.c
func calcOutputSingle(filter *sincFilter, increment, startFilterIndex incrementT) float64 {
	var left, right float64 // Use float64 for accumulators

	// C: max_filter_index = int_to_fp (filter->coeff_half_len) ;
	// Use the Go helper function. coeffHalfLen is already int.
	maxFilterIndex := intToFP(filter.coeffHalfLen)

	//---------------- Apply the left half of the filter --------------------
	// Initialize indices for the left side loop
	filterIndex := startFilterIndex
	// C: coeff_count = (max_filter_index - filter_index) / increment ;
	// Integer division using fixed-point types
	coeffCount := int((maxFilterIndex - filterIndex) / increment) // Result is int samples
	// C: filter_index = filter_index + coeff_count * increment ;
	filterIndex = filterIndex + incrementT(coeffCount)*increment // Start at the outermost tap used
	// C: data_index = filter->b_current - coeff_count ;
	dataIndex := filter.bCurrent - coeffCount // Corresponding index in the buffer

	// C: if (data_index < 0) { ... } // Handle buffer look-back extending before start
	if dataIndex < 0 {
		steps := -dataIndex // How many samples we need to step forward
		// C: assert (steps <= int_div_ceil (filter_index, increment)) ;
		// Go check: Panic if assertion fails, indicates logic error upstream.
		if increment <= 0 {
			panic("calcOutputSingle: increment must be positive")
		}
		maxSteps := intDivCeil(int(filterIndex), int(increment)) // Use positive values for intDivCeil
		if filterIndex < 0 {                                     // Handle case where filterIndex might start negative
			maxSteps = intDivCeil(int(-filterIndex+increment-1), int(increment))
		}
		if steps > maxSteps {
			panic(fmt.Sprintf("calcOutputSingle: buffer underflow assertion failed (steps=%d > maxSteps=%d, filterIndex=%d, increment=%d)", steps, maxSteps, filterIndex, increment))
		}
		filterIndex -= incrementT(steps) * increment
		dataIndex += steps // dataIndex should now be >= 0
	}

	left = 0.0
	// C: while (filter_index >= MAKE_INCREMENT_T (0)) { ... }
	for filterIndex >= 0 {
		// Calculate coefficient using linear interpolation between table entries
		// C: fraction = fp_to_double (filter_index) ;
		fraction := fpToDouble(filterIndex) // Fractional part as float64 [0.0, 1.0)
		// C: indx = fp_to_int (filter_index) ;
		indx := fpToInt(filterIndex) // Integer part (index into coeffs table)

		// Coefficient interpolation: C = C[i] + frac * (C[i+1] - C[i])
		// C: assert (indx >= 0 && indx + 1 < filter->coeff_half_len + 2) ;
		// Go bounds check: Accessing filter.coeffs[indx] and filter.coeffs[indx+1]
		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputSingle: coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		// Perform interpolation using float64 for precision
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// Accumulate: out += coeff * buffer_sample
		// C: assert (data_index >= 0 && data_index < filter->b_len) ;
		// C: assert (data_index < filter->b_end) ;
		// Go bounds check for buffer access: must be within allocated length AND valid data range
		if dataIndex < 0 || dataIndex >= filter.bLen {
			panic(fmt.Sprintf("calcOutputSingle: left buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex >= filter.bEnd { // Check against valid data end marker
			// Reading past valid data - indicates upstream logic error (prepareData or index calc)
			panic(fmt.Sprintf("calcOutputSingle: left buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}
		left += icoeff * float64(filter.buffer[dataIndex])

		// Update indices for next iteration
		filterIndex -= increment
		dataIndex++ // Move forward in buffer (older samples)
	}

	//---------------- Apply the right half of the filter -------------------
	// Initialize indices for the right side loop
	// C: filter_index = increment - start_filter_index ;
	filterIndex = increment - startFilterIndex // Start relative to the *next* sample's filter position
	// C: coeff_count = (max_filter_index - filter_index) / increment ;
	if filterIndex > maxFilterIndex {
		coeffCount = -1
	} else { // Avoid negative result if filterIndex > max
		coeffCount = int((maxFilterIndex - filterIndex) / increment)
	}
	// C: filter_index = filter_index + coeff_count * increment ;
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	// C: data_index = filter->b_current + 1 + coeff_count ;
	dataIndex = filter.bCurrent + 1 + coeffCount // Start at buffer index corresponding to tap

	right = 0.0
	// C: do { ... } while (filter_index > MAKE_INCREMENT_T (0)) ;
	for {
		// Calculate coefficient using linear interpolation
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)

		// C: assert (indx < filter->coeff_half_len + 2) ; => assert (indx < len(filter.coeffs))
		// Need indx+1 for interpolation, so check that too.
		// Also ensure indx is not negative, although logic should prevent it here.
		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputSingle: right coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// Accumulate: out += coeff * buffer_sample
		// C: assert (data_index >= 0 && data_index < filter->b_len) ;
		// C: assert (data_index < filter->b_end) ;
		// Go bounds check for buffer access
		if dataIndex < 0 || dataIndex >= filter.bLen {
			panic(fmt.Sprintf("calcOutputSingle: right buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputSingle: right buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}
		right += icoeff * float64(filter.buffer[dataIndex])

		// Update indices for next iteration
		filterIndex -= increment
		dataIndex-- // Move backward in buffer (newer samples)

		// Check loop condition C: while (filter_index > MAKE_INCREMENT_T (0))
		if !(filterIndex > 0) {
			break
		}
	} // End do-while loop

	// C: return (left + right) ;
	return left + right
}

// --- TODO: Implement Processing & Helper Functions ---

// sincMonoVariProcess handles mono data with potentially varying sample rate ratio.
// Corresponds to sinc_mono_vari_process in src_sinc.c (Corrected version)
func sincMonoVariProcess(state *srcState, data *SrcData) ErrorCode {
	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		return ErrBadState
	}
	if state.channels != 1 {
		return ErrBadInternalState
	}
	inputIndex := state.lastPosition
	srcRatio := state.lastRatio
	var increment, startFilterIndex incrementT
	var halfFilterChanLen, samplesInHand int
	outCountSamples := data.OutputFrames * int64(state.channels)
	data.InputFramesUsed = 0
	data.OutputFramesGen = 0
	var inUsedSamples int64 = 0
	var outGenSamples int64 = 0

	if isBadSrcRatio(srcRatio) {
		if isBadSrcRatio(data.SrcRatio) {
			return ErrBadSrcRatio
		}
		srcRatio = data.SrcRatio
		state.lastRatio = srcRatio
	}
	filterCoeffsLen := float64(filter.coeffHalfLen + 2)
	if filter.indexInc <= 0 {
		return ErrBadInternalState
	}
	count := filterCoeffsLen / float64(filter.indexInc)
	minRatio := minFloat64(state.lastRatio, data.SrcRatio)
	if minRatio < (1.0 / srcMaxRatio) {
		minRatio = 1.0 / srcMaxRatio
	}
	if minRatio < 1.0 && minRatio != 0 {
		count /= minRatio
	}
	halfFilterChanLen = state.channels * (psfLrint(count) + 1)
	intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex))
	if filter.bLen <= 0 {
		return ErrBadInternalState
	}
	filter.bCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
	inputIndex = fmodOne(inputIndex)

	for outGenSamples < outCountSamples {
		if filter.bEnd >= filter.bCurrent {
			samplesInHand = filter.bEnd - filter.bCurrent
		} else {
			samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
		}
		if samplesInHand <= halfFilterChanLen {
			data.InputFramesUsed = inUsedSamples / int64(state.channels)
			errCode := prepareData(filter, state.channels, data, halfFilterChanLen)
			if errCode != ErrNoError {
				state.errCode = errCode
				return errCode
			}
			inUsedSamples = data.InputFramesUsed * int64(state.channels)
			if filter.bEnd >= filter.bCurrent {
				samplesInHand = filter.bEnd - filter.bCurrent
			} else {
				samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
			}
			if samplesInHand <= halfFilterChanLen {
				break
			}
		}
		if filter.bRealEnd >= 0 {
			maxIndexNeeded := filter.bCurrent + halfFilterChanLen
			if maxIndexNeeded >= filter.bRealEnd {
				break
			}
		}
		if outCountSamples > 0 && math.Abs(state.lastRatio-data.SrcRatio) > srcMinRatioDiff {
			srcRatio = state.lastRatio + float64(outGenSamples)*(data.SrcRatio-state.lastRatio)/float64(outCountSamples)
			if isBadSrcRatio(srcRatio) {
				if srcRatio < 1.0/srcMaxRatio {
					srcRatio = 1.0 / srcMaxRatio
				}
				if srcRatio > srcMaxRatio {
					srcRatio = srcMaxRatio
				}
			}
		}
		floatIncrement := float64(filter.indexInc) * minFloat64(srcRatio, 1.0)
		increment = doubleToFP(floatIncrement)
		if increment == 0 {
			return ErrBadSrcRatio
		}
		startFilterIndex = doubleToFP(inputIndex * floatIncrement)
		scaleFactor := floatIncrement / float64(filter.indexInc)
		outputSample := scaleFactor * calcOutputSingle(filter, increment, startFilterIndex)
		if int(outGenSamples) >= len(data.DataOut) {
			return ErrBadData
		}
		data.DataOut[outGenSamples] = float32(outputSample)
		outGenSamples++
		if srcRatio == 0 {
			return ErrBadSrcRatio
		}
		inputIndex += 1.0 / srcRatio
		intInputAdvance = psfLrint(inputIndex - fmodOne(inputIndex))
		filter.bCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
		inputIndex = fmodOne(inputIndex)
	}
	state.lastPosition = inputIndex
	state.lastRatio = srcRatio
	data.OutputFramesGen = outGenSamples / int64(state.channels)
	data.InputFramesUsed = inUsedSamples / int64(state.channels)
	return ErrNoError
}

// calcOutputStereo calculates a pair of interpolated stereo output samples.
// Corresponds to calc_output_stereo in src_sinc.c
func calcOutputStereo(filter *sincFilter, channels int, increment, startFilterIndex incrementT, scale float64, output []float32) {
	// Ensure output slice has space for 2 samples
	if len(output) < 2 {
		panic(fmt.Sprintf("calcOutputStereo: output slice too small (len=%d, need 2)", len(output)))
	}
	// Ensure channels argument is correct (although likely always 2 when called)
	if channels != 2 {
		panic(fmt.Sprintf("calcOutputStereo called with incorrect channel count: %d", channels))
	}

	var left, right [2]float64 // Use float64 arrays for stereo accumulators

	maxFilterIndex := intToFP(filter.coeffHalfLen)

	//---------------- Apply the left half of the filter --------------------
	filterIndex := startFilterIndex
	coeffCount := int((maxFilterIndex - filterIndex) / increment)
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	// dataIndex steps by 'channels' (2) for each coefficient tap
	dataIndex := filter.bCurrent - channels*coeffCount

	if dataIndex < 0 {
		// Need to step forward; calculate steps based on channel pairs
		steps := intDivCeil(-dataIndex, channels) // Divide by channel count (2)

		// Assertion check
		if increment <= 0 {
			panic("calcOutputStereo: increment must be positive")
		}
		maxSteps := intDivCeil(int(filterIndex), int(increment))
		if filterIndex < 0 {
			maxSteps = intDivCeil(int(-filterIndex+increment-1), int(increment))
		}
		if steps > maxSteps {
			panic(fmt.Sprintf("calcOutputStereo: buffer underflow assertion failed (steps=%d > maxSteps=%d, filterIndex=%d, increment=%d)", steps, maxSteps, filterIndex, increment))
		}

		filterIndex -= incrementT(steps) * increment
		dataIndex += steps * channels // Step forward by sample pairs
	}

	left[0], left[1] = 0.0, 0.0
	for filterIndex >= 0 {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)

		// Coefficient bounds check
		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputStereo: left coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// Buffer bounds check (for both channels: dataIndex and dataIndex+1)
		if dataIndex < 0 || dataIndex+1 >= filter.bLen {
			panic(fmt.Sprintf("calcOutputStereo: left buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex+1 >= filter.bEnd { // Check against valid data end marker
			panic(fmt.Sprintf("calcOutputStereo: left buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}

		// Accumulate for both channels
		left[0] += icoeff * float64(filter.buffer[dataIndex])
		left[1] += icoeff * float64(filter.buffer[dataIndex+1])

		filterIndex -= increment
		dataIndex += channels // Move to the next sample pair (older data)
	}

	//---------------- Apply the right half of the filter -------------------
	filterIndex = increment - startFilterIndex
	if filterIndex > maxFilterIndex {
		coeffCount = -1
	} else {
		coeffCount = int((maxFilterIndex - filterIndex) / increment)
	}
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex = filter.bCurrent + channels*(1+coeffCount) // Start after center point

	right[0], right[1] = 0.0, 0.0
	for {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)

		// Coefficient bounds check
		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputStereo: right coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// Buffer bounds check (dataIndex and dataIndex+1)
		if dataIndex < 0 || dataIndex+1 >= filter.bLen {
			panic(fmt.Sprintf("calcOutputStereo: right buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex+1 >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputStereo: right buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}

		// Accumulate for both channels
		right[0] += icoeff * float64(filter.buffer[dataIndex])
		right[1] += icoeff * float64(filter.buffer[dataIndex+1])

		filterIndex -= increment
		dataIndex -= channels // Move to the previous sample pair (newer data)

		if !(filterIndex > 0) {
			break
		}
	} // End do-while loop

	// --- Combine, scale, and write output ---
	output[0] = float32(scale * (left[0] + right[0]))
	output[1] = float32(scale * (left[1] + right[1]))
}

// sincStereoVariProcess handles stereo data with potentially varying sample rate ratio.
// Corresponds to sinc_stereo_vari_process in src_sinc.c
func sincStereoVariProcess(state *srcState, data *SrcData) ErrorCode {
	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		return ErrBadState
	}
	if state.channels != 2 {
		return ErrBadInternalState
	}
	inputIndex := state.lastPosition
	srcRatio := state.lastRatio
	var increment, startFilterIndex incrementT
	var halfFilterChanLen, samplesInHand int
	outCountSamples := data.OutputFrames * int64(state.channels)
	data.InputFramesUsed = 0
	data.OutputFramesGen = 0
	var inUsedSamples int64 = 0
	var outGenSamples int64 = 0
	if isBadSrcRatio(srcRatio) {
		if isBadSrcRatio(data.SrcRatio) {
			return ErrBadSrcRatio
		}
		srcRatio = data.SrcRatio
		state.lastRatio = srcRatio
	}
	filterCoeffsLen := float64(filter.coeffHalfLen + 2)
	if filter.indexInc <= 0 {
		return ErrBadInternalState
	}
	count := filterCoeffsLen / float64(filter.indexInc)
	minRatio := minFloat64(state.lastRatio, data.SrcRatio)
	if minRatio < (1.0 / srcMaxRatio) {
		minRatio = 1.0 / srcMaxRatio
	}
	if minRatio < 1.0 && minRatio != 0 {
		count /= minRatio
	}
	halfFilterChanLen = state.channels * (psfLrint(count) + 1)
	intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex))
	if filter.bLen <= 0 {
		return ErrBadInternalState
	}
	filter.bCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
	inputIndex = fmodOne(inputIndex)
	for outGenSamples < outCountSamples {
		if filter.bEnd >= filter.bCurrent {
			samplesInHand = filter.bEnd - filter.bCurrent
		} else {
			samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
		}
		if samplesInHand <= halfFilterChanLen {
			data.InputFramesUsed = inUsedSamples / int64(state.channels)
			errCode := prepareData(filter, state.channels, data, halfFilterChanLen)
			if errCode != ErrNoError {
				state.errCode = errCode
				return errCode
			}
			inUsedSamples = data.InputFramesUsed * int64(state.channels)
			if filter.bEnd >= filter.bCurrent {
				samplesInHand = filter.bEnd - filter.bCurrent
			} else {
				samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
			}
			if samplesInHand <= halfFilterChanLen {
				break
			}
		}
		if filter.bRealEnd >= 0 {
			maxIndexNeeded := filter.bCurrent + halfFilterChanLen
			if maxIndexNeeded >= filter.bRealEnd {
				break
			}
		}
		if outCountSamples > 0 && math.Abs(state.lastRatio-data.SrcRatio) > srcMinRatioDiff {
			srcRatio = state.lastRatio + float64(outGenSamples)*(data.SrcRatio-state.lastRatio)/float64(outCountSamples)
			if isBadSrcRatio(srcRatio) {
				if srcRatio < 1.0/srcMaxRatio {
					srcRatio = 1.0 / srcMaxRatio
				}
				if srcRatio > srcMaxRatio {
					srcRatio = srcMaxRatio
				}
			}
		}
		floatIncrement := float64(filter.indexInc) * minFloat64(srcRatio, 1.0)
		increment = doubleToFP(floatIncrement)
		if increment == 0 {
			return ErrBadSrcRatio
		}
		startFilterIndex = doubleToFP(inputIndex * floatIncrement)
		scaleFactor := floatIncrement / float64(filter.indexInc)
		outPos := int(outGenSamples)
		if outPos+state.channels > len(data.DataOut) {
			break
		}
		outputSlice := data.DataOut[outPos : outPos+state.channels]
		calcOutputStereo(filter, state.channels, increment, startFilterIndex, scaleFactor, outputSlice)
		outGenSamples += int64(state.channels)
		if srcRatio == 0 {
			return ErrBadSrcRatio
		}
		inputIndex += 1.0 / srcRatio
		intInputAdvance = psfLrint(inputIndex - fmodOne(inputIndex))
		filter.bCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
		inputIndex = fmodOne(inputIndex)
	}
	state.lastPosition = inputIndex
	state.lastRatio = srcRatio
	data.OutputFramesGen = outGenSamples / int64(state.channels)
	data.InputFramesUsed = inUsedSamples / int64(state.channels)
	return ErrNoError
}

// calcOutputQuad calculates a set of 4 interpolated quad output samples.
// Corresponds to calc_output_quad in src_sinc.c
func calcOutputQuad(filter *sincFilter, channels int, increment, startFilterIndex incrementT, scale float64, output []float32) {
	// Ensure output slice has space for 4 samples
	if len(output) < 4 {
		panic(fmt.Sprintf("calcOutputQuad: output slice too small (len=%d, need 4)", len(output)))
	}
	// Ensure channels argument is correct
	if channels != 4 {
		panic(fmt.Sprintf("calcOutputQuad called with incorrect channel count: %d", channels))
	}

	var left, right [4]float64 // Use float64 arrays for quad accumulators

	maxFilterIndex := intToFP(filter.coeffHalfLen)

	//---------------- Apply the left half of the filter --------------------
	filterIndex := startFilterIndex
	coeffCount := int((maxFilterIndex - filterIndex) / increment)
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex := filter.bCurrent - channels*coeffCount

	if dataIndex < 0 {
		steps := intDivCeil(-dataIndex, channels) // Divide by 4

		if increment <= 0 {
			panic("calcOutputQuad: increment must be positive")
		}
		maxSteps := intDivCeil(int(filterIndex), int(increment))
		if filterIndex < 0 {
			maxSteps = intDivCeil(int(-filterIndex+increment-1), int(increment))
		}
		if steps > maxSteps {
			panic(fmt.Sprintf("calcOutputQuad: buffer underflow assertion failed (steps=%d > maxSteps=%d, filterIndex=%d, increment=%d)", steps, maxSteps, filterIndex, increment))
		}

		filterIndex -= incrementT(steps) * increment
		dataIndex += steps * channels // Step forward by sample groups of 4
	}

	// Initialize left accumulators
	left[0], left[1], left[2], left[3] = 0.0, 0.0, 0.0, 0.0
	for filterIndex >= 0 {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)

		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputQuad: left coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// Buffer bounds check (for all 4 channels: dataIndex to dataIndex+3)
		if dataIndex < 0 || dataIndex+3 >= filter.bLen {
			panic(fmt.Sprintf("calcOutputQuad: left buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex+3 >= filter.bEnd { // Check against valid data end marker
			panic(fmt.Sprintf("calcOutputQuad: left buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}

		// Accumulate for all 4 channels
		for ch := 0; ch < 4; ch++ {
			left[ch] += icoeff * float64(filter.buffer[dataIndex+ch])
		}

		filterIndex -= increment
		dataIndex += channels // Move to the next sample group (older data)
	}

	//---------------- Apply the right half of the filter -------------------
	filterIndex = increment - startFilterIndex
	if filterIndex > maxFilterIndex {
		coeffCount = -1
	} else {
		coeffCount = int((maxFilterIndex - filterIndex) / increment)
	}
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex = filter.bCurrent + channels*(1+coeffCount) // Start after center point

	// Initialize right accumulators
	right[0], right[1], right[2], right[3] = 0.0, 0.0, 0.0, 0.0
	for {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)

		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputQuad: right coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// Buffer bounds check (dataIndex to dataIndex+3)
		if dataIndex < 0 || dataIndex+3 >= filter.bLen {
			panic(fmt.Sprintf("calcOutputQuad: right buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex+3 >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputQuad: right buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}

		// Accumulate for all 4 channels
		for ch := 0; ch < 4; ch++ {
			right[ch] += icoeff * float64(filter.buffer[dataIndex+ch])
		}

		filterIndex -= increment
		dataIndex -= channels // Move to the previous sample group (newer data)

		if !(filterIndex > 0) {
			break
		}
	} // End do-while loop

	// --- Combine, scale, and write output ---
	for ch := 0; ch < 4; ch++ {
		output[ch] = float32(scale * (left[ch] + right[ch]))
	}
}

// sincQuadVariProcess handles quad audio data with potentially varying sample rate ratio.
// Corresponds to sinc_quad_vari_process in src_sinc.c
func sincQuadVariProcess(state *srcState, data *SrcData) ErrorCode {
	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		return ErrBadState
	}
	if state.channels != 4 {
		return ErrBadInternalState
	}
	inputIndex := state.lastPosition
	srcRatio := state.lastRatio
	var increment, startFilterIndex incrementT
	var halfFilterChanLen, samplesInHand int
	outCountSamples := data.OutputFrames * int64(state.channels)
	data.InputFramesUsed = 0
	data.OutputFramesGen = 0
	var inUsedSamples int64 = 0
	var outGenSamples int64 = 0
	if isBadSrcRatio(srcRatio) {
		if isBadSrcRatio(data.SrcRatio) {
			return ErrBadSrcRatio
		}
		srcRatio = data.SrcRatio
		state.lastRatio = srcRatio
	}
	filterCoeffsLen := float64(filter.coeffHalfLen + 2)
	if filter.indexInc <= 0 {
		return ErrBadInternalState
	}
	count := filterCoeffsLen / float64(filter.indexInc)
	minRatio := minFloat64(state.lastRatio, data.SrcRatio)
	if minRatio < (1.0 / srcMaxRatio) {
		minRatio = 1.0 / srcMaxRatio
	}
	if minRatio < 1.0 && minRatio != 0 {
		count /= minRatio
	}
	halfFilterChanLen = state.channels * (psfLrint(count) + 1)
	intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex))
	if filter.bLen <= 0 {
		return ErrBadInternalState
	}
	filter.bCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
	inputIndex = fmodOne(inputIndex)
	for outGenSamples < outCountSamples {
		if filter.bEnd >= filter.bCurrent {
			samplesInHand = filter.bEnd - filter.bCurrent
		} else {
			samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
		}
		if samplesInHand <= halfFilterChanLen {
			data.InputFramesUsed = inUsedSamples / int64(state.channels)
			errCode := prepareData(filter, state.channels, data, halfFilterChanLen)
			if errCode != ErrNoError {
				state.errCode = errCode
				return errCode
			}
			inUsedSamples = data.InputFramesUsed * int64(state.channels)
			if filter.bEnd >= filter.bCurrent {
				samplesInHand = filter.bEnd - filter.bCurrent
			} else {
				samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
			}
			if samplesInHand <= halfFilterChanLen {
				break
			}
		}
		if filter.bRealEnd >= 0 {
			maxIndexNeeded := filter.bCurrent + halfFilterChanLen
			if maxIndexNeeded >= filter.bRealEnd {
				break
			}
		}
		if outCountSamples > 0 && math.Abs(state.lastRatio-data.SrcRatio) > srcMinRatioDiff {
			srcRatio = state.lastRatio + float64(outGenSamples)*(data.SrcRatio-state.lastRatio)/float64(outCountSamples)
			if isBadSrcRatio(srcRatio) {
				if srcRatio < 1.0/srcMaxRatio {
					srcRatio = 1.0 / srcMaxRatio
				}
				if srcRatio > srcMaxRatio {
					srcRatio = srcMaxRatio
				}
			}
		}
		floatIncrement := float64(filter.indexInc) * minFloat64(srcRatio, 1.0)
		increment = doubleToFP(floatIncrement)
		if increment == 0 {
			return ErrBadSrcRatio
		}
		startFilterIndex = doubleToFP(inputIndex * floatIncrement)
		scaleFactor := floatIncrement / float64(filter.indexInc)
		outPos := int(outGenSamples)
		if outPos+state.channels > len(data.DataOut) {
			break
		}
		outputSlice := data.DataOut[outPos : outPos+state.channels]
		calcOutputQuad(filter, state.channels, increment, startFilterIndex, scaleFactor, outputSlice)
		outGenSamples += int64(state.channels)
		if srcRatio == 0 {
			return ErrBadSrcRatio
		}
		inputIndex += 1.0 / srcRatio
		intInputAdvance = psfLrint(inputIndex - fmodOne(inputIndex))
		filter.bCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
		inputIndex = fmodOne(inputIndex)
	}
	state.lastPosition = inputIndex
	state.lastRatio = srcRatio
	data.OutputFramesGen = outGenSamples / int64(state.channels)
	data.InputFramesUsed = inUsedSamples / int64(state.channels)
	return ErrNoError
}

// calcOutputHex calculates a set of 6 interpolated hex output samples.
// Corresponds to calc_output_hex in src_sinc.c
func calcOutputHex(filter *sincFilter, channels int, increment, startFilterIndex incrementT, scale float64, output []float32) {
	// Ensure output slice has space for 6 samples
	if len(output) < 6 {
		panic(fmt.Sprintf("calcOutputHex: output slice too small (len=%d, need 6)", len(output)))
	}
	// Ensure channels argument is correct
	if channels != 6 {
		panic(fmt.Sprintf("calcOutputHex called with incorrect channel count: %d", channels))
	}

	var left, right [6]float64 // Use float64 arrays for hex accumulators

	maxFilterIndex := intToFP(filter.coeffHalfLen)

	//---------------- Apply the left half of the filter --------------------
	filterIndex := startFilterIndex
	coeffCount := int((maxFilterIndex - filterIndex) / increment)
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex := filter.bCurrent - channels*coeffCount // channels = 6

	if dataIndex < 0 {
		steps := intDivCeil(-dataIndex, channels) // Divide by 6

		if increment <= 0 {
			panic("calcOutputHex: increment must be positive")
		}
		maxSteps := intDivCeil(int(filterIndex), int(increment))
		if filterIndex < 0 {
			maxSteps = intDivCeil(int(-filterIndex+increment-1), int(increment))
		}
		if steps > maxSteps {
			panic(fmt.Sprintf("calcOutputHex: buffer underflow assertion failed (steps=%d > maxSteps=%d, filterIndex=%d, increment=%d)", steps, maxSteps, filterIndex, increment))
		}

		filterIndex -= incrementT(steps) * increment
		dataIndex += steps * channels // Step forward by sample groups of 6
	}

	// Initialize left accumulators
	for ch := 0; ch < 6; ch++ {
		left[ch] = 0.0
	}
	for filterIndex >= 0 {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)

		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputHex: left coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// Buffer bounds check (for all 6 channels: dataIndex to dataIndex+5)
		if dataIndex < 0 || dataIndex+5 >= filter.bLen {
			panic(fmt.Sprintf("calcOutputHex: left buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex+5 >= filter.bEnd { // Check against valid data end marker
			panic(fmt.Sprintf("calcOutputHex: left buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}

		// Accumulate for all 6 channels
		for ch := 0; ch < 6; ch++ {
			left[ch] += icoeff * float64(filter.buffer[dataIndex+ch])
		}

		filterIndex -= increment
		dataIndex += channels // Move to the next sample group (older data)
	}

	//---------------- Apply the right half of the filter -------------------
	filterIndex = increment - startFilterIndex
	if filterIndex > maxFilterIndex {
		coeffCount = -1
	} else {
		coeffCount = int((maxFilterIndex - filterIndex) / increment)
	}
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex = filter.bCurrent + channels*(1+coeffCount) // Start after center point

	// Initialize right accumulators
	for ch := 0; ch < 6; ch++ {
		right[ch] = 0.0
	}
	for {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)

		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputHex: right coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// Buffer bounds check (dataIndex to dataIndex+5)
		if dataIndex < 0 || dataIndex+5 >= filter.bLen {
			panic(fmt.Sprintf("calcOutputHex: right buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex+5 >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputHex: right buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}

		// Accumulate for all 6 channels
		for ch := 0; ch < 6; ch++ {
			right[ch] += icoeff * float64(filter.buffer[dataIndex+ch])
		}

		filterIndex -= increment
		dataIndex -= channels // Move to the previous sample group (newer data)

		if !(filterIndex > 0) {
			break
		}
	} // End do-while loop

	// --- Combine, scale, and write output ---
	for ch := 0; ch < 6; ch++ {
		output[ch] = float32(scale * (left[ch] + right[ch]))
	}
}

// sincHexVariProcess handles 6-channel audio data with potentially varying sample rate ratio.
// Corresponds to sinc_hex_vari_process in src_sinc.c
func sincHexVariProcess(state *srcState, data *SrcData) ErrorCode {
	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		return ErrBadState
	}
	if state.channels != 6 {
		return ErrBadInternalState
	}
	inputIndex := state.lastPosition
	srcRatio := state.lastRatio
	var increment, startFilterIndex incrementT
	var halfFilterChanLen, samplesInHand int
	outCountSamples := data.OutputFrames * int64(state.channels)
	data.InputFramesUsed = 0
	data.OutputFramesGen = 0
	var inUsedSamples int64 = 0
	var outGenSamples int64 = 0
	if isBadSrcRatio(srcRatio) {
		if isBadSrcRatio(data.SrcRatio) {
			return ErrBadSrcRatio
		}
		srcRatio = data.SrcRatio
		state.lastRatio = srcRatio
	}
	filterCoeffsLen := float64(filter.coeffHalfLen + 2)
	if filter.indexInc <= 0 {
		return ErrBadInternalState
	}
	count := filterCoeffsLen / float64(filter.indexInc)
	minRatio := minFloat64(state.lastRatio, data.SrcRatio)
	if minRatio < (1.0 / srcMaxRatio) {
		minRatio = 1.0 / srcMaxRatio
	}
	if minRatio < 1.0 && minRatio != 0 {
		count /= minRatio
	}
	halfFilterChanLen = state.channels * (psfLrint(count) + 1)
	intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex))
	if filter.bLen <= 0 {
		return ErrBadInternalState
	}
	filter.bCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
	inputIndex = fmodOne(inputIndex)
	for outGenSamples < outCountSamples {
		if filter.bEnd >= filter.bCurrent {
			samplesInHand = filter.bEnd - filter.bCurrent
		} else {
			samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
		}
		if samplesInHand <= halfFilterChanLen {
			data.InputFramesUsed = inUsedSamples / int64(state.channels)
			errCode := prepareData(filter, state.channels, data, halfFilterChanLen)
			if errCode != ErrNoError {
				state.errCode = errCode
				return errCode
			}
			inUsedSamples = data.InputFramesUsed * int64(state.channels)
			if filter.bEnd >= filter.bCurrent {
				samplesInHand = filter.bEnd - filter.bCurrent
			} else {
				samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
			}
			if samplesInHand <= halfFilterChanLen {
				break
			}
		}
		if filter.bRealEnd >= 0 {
			maxIndexNeeded := filter.bCurrent + halfFilterChanLen
			if maxIndexNeeded >= filter.bRealEnd {
				break
			}
		}
		if outCountSamples > 0 && math.Abs(state.lastRatio-data.SrcRatio) > srcMinRatioDiff {
			srcRatio = state.lastRatio + float64(outGenSamples)*(data.SrcRatio-state.lastRatio)/float64(outCountSamples)
			if isBadSrcRatio(srcRatio) {
				if srcRatio < 1.0/srcMaxRatio {
					srcRatio = 1.0 / srcMaxRatio
				}
				if srcRatio > srcMaxRatio {
					srcRatio = srcMaxRatio
				}
			}
		}
		floatIncrement := float64(filter.indexInc) * minFloat64(srcRatio, 1.0)
		increment = doubleToFP(floatIncrement)
		if increment == 0 {
			return ErrBadSrcRatio
		}
		startFilterIndex = doubleToFP(inputIndex * floatIncrement)
		scaleFactor := floatIncrement / float64(filter.indexInc)
		outPos := int(outGenSamples)
		if outPos+state.channels > len(data.DataOut) {
			break
		}
		outputSlice := data.DataOut[outPos : outPos+state.channels]
		calcOutputHex(filter, state.channels, increment, startFilterIndex, scaleFactor, outputSlice)
		outGenSamples += int64(state.channels)
		if srcRatio == 0 {
			return ErrBadSrcRatio
		}
		inputIndex += 1.0 / srcRatio
		intInputAdvance = psfLrint(inputIndex - fmodOne(inputIndex))
		filter.bCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
		inputIndex = fmodOne(inputIndex)
	}
	state.lastPosition = inputIndex
	state.lastRatio = srcRatio
	data.OutputFramesGen = outGenSamples / int64(state.channels)
	data.InputFramesUsed = inUsedSamples / int64(state.channels)
	return ErrNoError
}

// calcOutputMulti calculates a set of interpolated output samples for multiple channels.
// Corresponds to calc_output_multi in src_sinc.c (Now Implemented)
func calcOutputMulti(filter *sincFilter, channels int, increment, startFilterIndex incrementT, scale float64, output []float32) {
	if len(output) < channels {
		panic(fmt.Sprintf("calcOutputMulti: output slice too small (len=%d, need %d)", len(output), channels))
	}
	// Ensure scratch arrays are large enough (should be maxChannels)
	if channels > maxChannels {
		panic(fmt.Sprintf("calcOutputMulti: channel count %d exceeds maxChannels %d", channels, maxChannels))
	}

	// Use the scratch arrays from the filter struct
	left := filter.leftCalc[:channels]   // Slice to the number of channels needed
	right := filter.rightCalc[:channels] // Slice to the number of channels needed

	// Zero out scratch arrays before accumulating
	for ch := 0; ch < channels; ch++ {
		left[ch] = 0.0
		right[ch] = 0.0
	}

	maxFilterIndex := intToFP(filter.coeffHalfLen)

	//---------------- Apply the left half of the filter --------------------
	filterIndex := startFilterIndex
	coeffCount := int((maxFilterIndex - filterIndex) / increment)
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex := filter.bCurrent - channels*coeffCount

	if dataIndex < 0 {
		steps := intDivCeil(-dataIndex, channels)
		if increment <= 0 {
			panic("calcOutputMulti: increment must be positive")
		}
		maxSteps := intDivCeil(int(filterIndex), int(increment))
		if filterIndex < 0 {
			maxSteps = intDivCeil(int(-filterIndex+increment-1), int(increment))
		}
		if steps > maxSteps {
			panic(fmt.Sprintf("calcOutputMulti: buffer underflow assertion failed (steps=%d > maxSteps=%d, filterIndex=%d, increment=%d)", steps, maxSteps, filterIndex, increment))
		}
		filterIndex -= incrementT(steps) * increment
		dataIndex += steps * channels
	}

	for filterIndex >= 0 {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)
		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputMulti: left coeff index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		endDataIdx := dataIndex + channels - 1
		if dataIndex < 0 || endDataIdx >= filter.bLen {
			panic(fmt.Sprintf("calcOutputMulti: left buffer index out of allocated bounds (dataIndex=%d, channels=%d, bLen=%d)", dataIndex, channels, filter.bLen))
		}
		if endDataIdx >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputMulti: left buffer index out of valid data range (dataIndex=%d, channels=%d, bEnd=%d)", dataIndex, channels, filter.bEnd))
		}

		for ch := 0; ch < channels; ch++ {
			left[ch] += icoeff * float64(filter.buffer[dataIndex+ch])
		}
		filterIndex -= increment
		dataIndex += channels
	}

	//---------------- Apply the right half of the filter -------------------
	filterIndex = increment - startFilterIndex
	if filterIndex > maxFilterIndex {
		coeffCount = -1
	} else {
		coeffCount = int((maxFilterIndex - filterIndex) / increment)
	}
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex = filter.bCurrent + channels*(1+coeffCount)

	// Right accumulators already zeroed
	for {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)
		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputMulti: right coeff index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		endDataIdx := dataIndex + channels - 1
		if dataIndex < 0 || endDataIdx >= filter.bLen {
			panic(fmt.Sprintf("calcOutputMulti: right buffer index out of allocated bounds (dataIndex=%d, channels=%d, bLen=%d)", dataIndex, channels, filter.bLen))
		}
		if endDataIdx >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputMulti: right buffer index out of valid data range (dataIndex=%d, channels=%d, bEnd=%d)", dataIndex, channels, filter.bEnd))
		}

		for ch := 0; ch < channels; ch++ {
			right[ch] += icoeff * float64(filter.buffer[dataIndex+ch])
		}
		filterIndex -= increment
		dataIndex -= channels
		if !(filterIndex > 0) {
			break
		}
	}

	// --- Combine, scale, and write output ---
	for ch := 0; ch < channels; ch++ {
		output[ch] = float32(scale * (left[ch] + right[ch]))
	}
}

// sincMultichanVariProcess handles generic multi-channel audio data.
// Corresponds to sinc_multichan_vari_process in src_sinc.c
func sincMultichanVariProcess(state *srcState, data *SrcData) ErrorCode {
	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		return ErrBadState
	}
	// No specific channel check here
	inputIndex := state.lastPosition
	srcRatio := state.lastRatio
	var increment, startFilterIndex incrementT
	var halfFilterChanLen, samplesInHand int
	outCountSamples := data.OutputFrames * int64(state.channels)
	data.InputFramesUsed = 0
	data.OutputFramesGen = 0
	var inUsedSamples int64 = 0
	var outGenSamples int64 = 0
	if isBadSrcRatio(srcRatio) {
		if isBadSrcRatio(data.SrcRatio) {
			return ErrBadSrcRatio
		}
		srcRatio = data.SrcRatio
		state.lastRatio = srcRatio
	}
	filterCoeffsLen := float64(filter.coeffHalfLen + 2)
	if filter.indexInc <= 0 {
		return ErrBadInternalState
	}
	count := filterCoeffsLen / float64(filter.indexInc)
	minRatio := minFloat64(state.lastRatio, data.SrcRatio)
	if minRatio < (1.0 / srcMaxRatio) {
		minRatio = 1.0 / srcMaxRatio
	}
	if minRatio < 1.0 && minRatio != 0 {
		count /= minRatio
	}
	halfFilterChanLen = state.channels * (psfLrint(count) + 1)
	intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex))
	if filter.bLen <= 0 {
		return ErrBadInternalState
	}
	filter.bCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
	inputIndex = fmodOne(inputIndex)
	for outGenSamples < outCountSamples {
		if filter.bEnd >= filter.bCurrent {
			samplesInHand = filter.bEnd - filter.bCurrent
		} else {
			samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
		}
		if samplesInHand <= halfFilterChanLen {
			data.InputFramesUsed = inUsedSamples / int64(state.channels)
			errCode := prepareData(filter, state.channels, data, halfFilterChanLen)
			if errCode != ErrNoError {
				state.errCode = errCode
				return errCode
			}
			inUsedSamples = data.InputFramesUsed * int64(state.channels)
			if filter.bEnd >= filter.bCurrent {
				samplesInHand = filter.bEnd - filter.bCurrent
			} else {
				samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
			}
			if samplesInHand <= halfFilterChanLen {
				break
			}
		}
		if filter.bRealEnd >= 0 {
			maxIndexNeeded := filter.bCurrent + halfFilterChanLen
			if maxIndexNeeded >= filter.bRealEnd {
				break
			}
		}
		if outCountSamples > 0 && math.Abs(state.lastRatio-data.SrcRatio) > srcMinRatioDiff {
			srcRatio = state.lastRatio + float64(outGenSamples)*(data.SrcRatio-state.lastRatio)/float64(outCountSamples)
			if isBadSrcRatio(srcRatio) {
				if srcRatio < 1.0/srcMaxRatio {
					srcRatio = 1.0 / srcMaxRatio
				}
				if srcRatio > srcMaxRatio {
					srcRatio = srcMaxRatio
				}
			}
		}
		floatIncrement := float64(filter.indexInc) * minFloat64(srcRatio, 1.0)
		increment = doubleToFP(floatIncrement)
		if increment == 0 {
			return ErrBadSrcRatio
		}
		startFilterIndex = doubleToFP(inputIndex * floatIncrement)
		scaleFactor := floatIncrement / float64(filter.indexInc)
		outPos := int(outGenSamples)
		if outPos+state.channels > len(data.DataOut) {
			break
		}
		outputSlice := data.DataOut[outPos : outPos+state.channels]
		// *** Call calcOutputMulti ***
		calcOutputMulti(filter, state.channels, increment, startFilterIndex, scaleFactor, outputSlice)
		outGenSamples += int64(state.channels)
		if srcRatio == 0 {
			return ErrBadSrcRatio
		}
		inputIndex += 1.0 / srcRatio
		intInputAdvance = psfLrint(inputIndex - fmodOne(inputIndex))
		filter.bCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
		inputIndex = fmodOne(inputIndex)
	}
	state.lastPosition = inputIndex
	state.lastRatio = srcRatio
	data.OutputFramesGen = outGenSamples / int64(state.channels)
	data.InputFramesUsed = inUsedSamples / int64(state.channels)
	return ErrNoError
}

// --- End of Sinc Processing Functions ---

// Translate src_linear.c and src_zoh.c if those converters are desired.
// Ensure the main dispatcher psrcSetConverter correctly calls the constructors for all implemented types (newSincState, newLinearState, newZohState).
