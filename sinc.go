package libsamplerate

import (
	"fmt"
	"math"
	"os"
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

// Check environment variable ONCE at package initialization
var sincDebugEnabled = (os.Getenv("SINC_DEBUG") == "1")

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
	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil { /* handle error */
		return
	}

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincReset: Resetting filter state.\n")
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

	for i := range filter.leftCalc {
		filter.leftCalc[i] = 0.0
		filter.rightCalc[i] = 0.0 // Assuming rightCalc has same size based on maxChannels
	}

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

// sincResetX resets the internal state of the Sinc filter.
func sincResetX(state *srcState) {
	if state == nil || state.privateData == nil {
		return
	}
	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincReset: ERROR: PrivateData is not a valid *sincFilter (type: %T)\n", state.privateData)
		}
		return
	}

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincReset: Resetting filter state.\n")
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
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincClose: Closing filter state.\n")
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
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincCopy: ERROR: Original state has invalid private data.\n")
		}
		return nil
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincCopy: Copying filter state.\n")
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
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] prepareData: ENTRY - bCurrent=%d, bEnd=%d, bLen=%d, bRealEnd=%d, halfFCLen=%d, data.InFrames=%d, data.InUsed=%d, data.EOF=%t\n",
			filter.bCurrent, filter.bEnd, filter.bLen, filter.bRealEnd, halfFilterChanLen, data.InputFrames, data.InputFramesUsed, data.EndOfInput)
	}

	// Ensure valid input indices from SrcData
	// C uses filter->in_count, filter->in_used directly, but we rely on SrcData fields
	inCount := data.InputFrames * int64(channels)
	initialInUsedSamples := data.InputFramesUsed * int64(channels) // Use local var to track consumption *within* this call
	inUsedSamples := initialInUsedSamples

	// C: if (filter->b_real_end >= 0) return SRC_ERR_NO_ERROR;
	if filter.bRealEnd >= 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: bRealEnd (%d) >= 0, returning early.\n", filter.bRealEnd)
		}
		return ErrNoError // Already marked end-of-input, don't load more.
	}

	// C: if (data->data_in == NULL) return SRC_ERR_NO_ERROR;
	// In Go, check slice length. If InputFrames > 0, DataIn must be valid.
	if data.InputFrames > 0 && len(data.DataIn) == 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: ERROR: data.InputFrames > 0 but len(data.DataIn) == 0\n")
		}
		return ErrBadDataPtr
	}
	// If no new input frames are available *relative to what's already been used*, we also don't need to load.
	if inUsedSamples >= inCount && !data.EndOfInput {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: No more frames in current block and not EOF, returning.\n")
		}
		return ErrNoError // Nothing to load right now.
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] prepareData: Passed initial checks.\n")
	}

	currentDataOffset := int(inUsedSamples) // Start offset in data.DataIn slice

	var requiredLen int // How many samples we need to load

	// Buffer management logic:
	if filter.bCurrent == 0 { // C checks b_current == 0 for initial fill
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: Initial buffer fill case (bCurrent == 0).\n")
		}
		// Initial state or after wrap-around where bCurrent is reset to 0 implicitly by % op.
		// C code uses b_current==0 specifically for *initial* state setup.
		requiredLen = filter.bLen - (2 * halfFilterChanLen)
		if requiredLen < 0 {
			requiredLen = 0
		} // Ensure non-negative

		filter.bCurrent = halfFilterChanLen
		filter.bEnd = halfFilterChanLen
		// Buffer from 0 to halfFilterChanLen-1 is implicitly zero (from make or reset)

	} else if filter.bEnd+halfFilterChanLen+channels < filter.bLen {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: Enough space at buffer end case.\n")
		}
		// Enough space at the end of the buffer to load new data directly.
		availableSpaceAtEnd := filter.bLen - filter.bEnd
		requiredLen = availableSpaceAtEnd // Max we can load without wrapping

	} else {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: Buffer wrap case. bEnd=%d, bCurrent=%d, halfFCLen=%d, bLen=%d\n", filter.bEnd, filter.bCurrent, halfFilterChanLen, filter.bLen)
		}
		// Need to wrap data from the end back to the beginning.
		validDataLen := filter.bEnd - filter.bCurrent
		if validDataLen < 0 {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] prepareData: ERROR: Buffer wrap case detected bEnd (%d) < bCurrent (%d)\n", filter.bEnd, filter.bCurrent)
			}
			return ErrBadInternalState // Should not happen
		}

		srcStart := filter.bCurrent - halfFilterChanLen // Start of data to preserve (incl lookback)
		copyLen := halfFilterChanLen + validDataLen

		// Bounds checks for source slice
		if srcStart < 0 {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] prepareData: ERROR: Buffer wrap srcStart (%d) < 0\n", srcStart)
			}
			return ErrBadInternalState
		}
		if srcStart+copyLen > len(filter.buffer) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] prepareData: ERROR: Buffer wrap src bounds error: srcStart=%d, copyLen=%d, bufLen=%d\n", srcStart, copyLen, len(filter.buffer))
			}
			return ErrBadInternalState // Trying to copy past end of allocated buffer
		}

		// Check destination bounds (copying to start of buffer)
		if copyLen > len(filter.buffer) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] prepareData: ERROR: Buffer wrap dest bounds error: copyLen=%d, bufLen=%d\n", copyLen, len(filter.buffer))
			}
			return ErrBadInternalState // Cannot copy more than buffer size
		}

		// Perform the copy using Go's copy function (handles overlap)
		copy(filter.buffer[0:copyLen], filter.buffer[srcStart:srcStart+copyLen])
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: Wrapped %d samples from %d to 0.\n", copyLen, srcStart)
		}

		filter.bCurrent = halfFilterChanLen          // New read position is after preserved lookback
		filter.bEnd = filter.bCurrent + validDataLen // New write position is after copied valid data

		// Now calculate how much space is available to load new data after the wrap
		spaceToLoad := filter.bLen - filter.bEnd
		requiredLen = spaceToLoad // Max we can load now
	}

	// Now, determine how much data to actually copy from input
	// C: len = MIN ((int) (filter->in_count - filter->in_used), len) ;
	framesAvailable := inCount - inUsedSamples // Use current inUsedSamples
	if framesAvailable < 0 {
		framesAvailable = 0
	}

	copyCount := minInt(int(framesAvailable), requiredLen) // Samples to copy
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] prepareData: requiredLen=%d, framesAvailable=%d, decided copyCount=%d (before mod channels).\n", requiredLen, framesAvailable, copyCount)
	}

	// C: len -= (len % channels) ; // Ensure whole frames
	copyCount -= copyCount % channels
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] prepareData: final copyCount=%d (after mod channels).\n", copyCount)
	}

	// C: if (len < 0 || filter->b_end + len > filter->b_len) return SRC_ERR_SINC_PREPARE_DATA_BAD_LEN;
	if copyCount < 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: ERROR: final copyCount (%d) < 0\n", copyCount)
		}
		return ErrSincPrepareDataBadLen // Cannot copy negative amount
	}
	if filter.bEnd+copyCount > filter.bLen {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: ERROR: copy exceeds bLen: bEnd=%d, copyCount=%d, bLen=%d\n", filter.bEnd, copyCount, filter.bLen)
		}
		return ErrSincPrepareDataBadLen
	}
	if filter.bEnd+copyCount > len(filter.buffer) {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: ERROR: copy exceeds allocLen: bEnd=%d, copyCount=%d, allocLen=%d\n", filter.bEnd, copyCount, len(filter.buffer))
		}
		return ErrSincPrepareDataBadLen // Cannot write past allocated buffer
	}

	// Perform the copy from input data to internal buffer if there's data to copy
	if copyCount > 0 {
		// C: memcpy (filter->buffer + filter->b_end, data->data_in + filter->in_used, len * sizeof (filter->buffer [0])) ;
		// Ensure source slice bounds are okay
		if currentDataOffset+copyCount > len(data.DataIn) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] prepareData: ERROR: copy source bounds: offset=%d, copyCount=%d, len(DataIn)=%d\n", currentDataOffset, copyCount, len(data.DataIn))
			}
			return ErrBadData // Trying to read past end of provided input slice
		}

		copy(filter.buffer[filter.bEnd:filter.bEnd+copyCount], data.DataIn[currentDataOffset:currentDataOffset+copyCount])
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: Copied %d samples from input@%d to buffer@%d.\n", copyCount, currentDataOffset, filter.bEnd)
		}

		filter.bEnd += copyCount
		inUsedSamples += int64(copyCount) // Update local count
	} else {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: No samples copied (copyCount=0).\n")
		}
	}

	// Update SrcData with consumed frames (convert samples back to frames)
	// Only update if more was consumed than initially passed in
	if inUsedSamples > initialInUsedSamples {
		data.InputFramesUsed = inUsedSamples / int64(channels)
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: Updated data.InputFramesUsed to %d.\n", data.InputFramesUsed)
		}
	} else {
		// Ensure it reflects at least what was passed in if no new data consumed
		data.InputFramesUsed = initialInUsedSamples / int64(channels)
	}

	// Handle End Of Input: Add zero padding if needed.
	inputFullyConsumed := (inUsedSamples >= inCount) // Check if all *provided* input is used
	currentBufferSamples := filter.bEnd - filter.bCurrent
	if currentBufferSamples < 0 { // Handle wrap case for this check
		currentBufferSamples += filter.bLen
	}
	hasEnoughLookaround := currentBufferSamples >= (2 * halfFilterChanLen)

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] prepareData: EOF Check: inputFullyConsumed=%t, data.EOF=%t, hasEnoughLookaround=%t (currentBufferSamples=%d vs needed=%d)\n",
			inputFullyConsumed, data.EndOfInput, hasEnoughLookaround, currentBufferSamples, 2*halfFilterChanLen)
	}

	if inputFullyConsumed && data.EndOfInput && !hasEnoughLookaround {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: Entering EOF padding logic.\n")
		}

		// C first checks if buffer needs wrapping *again* before padding
		requiredPaddingSpace := halfFilterChanLen + 5 // C uses +5 margin?
		if filter.bLen-filter.bEnd < requiredPaddingSpace {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] prepareData: Wrapping buffer before padding. bEnd=%d, bLen=%d, reqSpace=%d\n", filter.bEnd, filter.bLen, requiredPaddingSpace)
			}

			validDataLen := filter.bEnd - filter.bCurrent
			if validDataLen < 0 {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] prepareData: ERROR: EOF pad-wrap detected bEnd (%d) < bCurrent (%d)\n", filter.bEnd, filter.bCurrent)
				}
				return ErrBadInternalState
			}

			srcStart := filter.bCurrent - halfFilterChanLen
			copyLen := halfFilterChanLen + validDataLen

			// Bounds checks (similar to above wrap logic)
			if srcStart < 0 {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] prepareData: ERROR: EOF pad-wrap srcStart (%d) < 0\n", srcStart)
				}
				return ErrBadInternalState
			}
			if srcStart+copyLen > len(filter.buffer) {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] prepareData: ERROR: EOF pad-wrap src bounds: srcStart=%d, copyLen=%d, bufLen=%d\n", srcStart, copyLen, len(filter.buffer))
				}
				return ErrBadInternalState
			}
			if copyLen > len(filter.buffer) {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] prepareData: ERROR: EOF pad-wrap dest bounds: copyLen=%d, bufLen=%d\n", copyLen, len(filter.buffer))
				}
				return ErrBadInternalState
			}

			copy(filter.buffer[0:copyLen], filter.buffer[srcStart:srcStart+copyLen])
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] prepareData: EOF pad-wrap copied %d samples from %d to 0.\n", copyLen, srcStart)
			}

			filter.bCurrent = halfFilterChanLen
			filter.bEnd = filter.bCurrent + validDataLen
		}

		// Now add zero padding
		filter.bRealEnd = filter.bEnd // Mark the end of actual data
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: Set bRealEnd = %d.\n", filter.bRealEnd)
		}

		paddingLen := halfFilterChanLen + 5 // Padding amount

		// Ensure padding doesn't exceed usable buffer length (bLen)
		if paddingLen < 0 {
			paddingLen = 0
		}
		if filter.bEnd+paddingLen > filter.bLen {
			paddingLen = filter.bLen - filter.bEnd
		}
		// Also ensure padding doesn't exceed allocated length
		if filter.bEnd+paddingLen > len(filter.buffer) {
			paddingLen = len(filter.buffer) - filter.bEnd
		}
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] prepareData: Calculated paddingLen = %d.\n", paddingLen)
		}

		if paddingLen > 0 {
			paddingSlice := filter.buffer[filter.bEnd : filter.bEnd+paddingLen]
			for i := range paddingSlice {
				paddingSlice[i] = 0.0
			}
			filter.bEnd += paddingLen
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] prepareData: Padded %d zeros. New bEnd = %d.\n", paddingLen, filter.bEnd)
			}
		}
	}

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] prepareData: EXIT - bCurrent=%d, bEnd=%d, bRealEnd=%d, data.InUsed=%d. Returning ErrNoError\n", filter.bCurrent, filter.bEnd, filter.bRealEnd, data.InputFramesUsed)
	}
	return ErrNoError
}

// calcOutputSingle calculates a single interpolated output sample.
// Corresponds to calc_output_single in src_sinc.c
func calcOutputSingle(filter *sincFilter, increment, startFilterIndex incrementT) float64 {
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] calcOutputSingle: ENTRY - increment=%d, startFilterIndex=%d, bCurrent=%d, bEnd=%d, bRealEnd=%d\n", increment, startFilterIndex, filter.bCurrent, filter.bEnd, filter.bRealEnd)
	}

	var left, right float64 // Use float64 for accumulators

	maxFilterIndex := intToFP(filter.coeffHalfLen)

	//---------------- Apply the left half of the filter --------------------
	filterIndex := startFilterIndex
	if increment <= 0 {
		panic(fmt.Sprintf("calcOutputSingle: invalid increment %d", increment))
	}
	coeffCount := int((maxFilterIndex - filterIndex) / increment)
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex := filter.bCurrent - coeffCount

	if dataIndex < 0 {
		steps := intDivCeil(-dataIndex, 1) // For single channel, step is 1
		maxSteps := intDivCeil(int(filterIndex), int(increment))
		if filterIndex < 0 {
			maxSteps = intDivCeil(int(-filterIndex+increment-1), int(increment))
		}
		if steps > maxSteps {
			panic(fmt.Sprintf("calcOutputSingle: buffer underflow assertion failed (steps=%d > maxSteps=%d, filterIndex=%d, increment=%d)", steps, maxSteps, filterIndex, increment))
		}
		filterIndex -= incrementT(steps) * increment
		dataIndex += steps
	}

	left = 0.0
	// loopCountLeft := 0
	for filterIndex >= 0 {
		// loopCountLeft++
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)

		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputSingle: left coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// --- NEW Checks and Read (Left Loop) ---
		var sampleValue float64 = 0.0 // Default to 0.0 (for padded area)
		if dataIndex < 0 || dataIndex >= filter.bLen {
			panic(fmt.Sprintf("calcOutputSingle: left buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		// Only read from buffer if within valid data range (before bEnd)
		// AND within the real data range (before bRealEnd, if EOF is marked)
		if dataIndex < filter.bEnd && (filter.bRealEnd < 0 || dataIndex < filter.bRealEnd) {
			sampleValue = float64(filter.buffer[dataIndex]) // Read real data
		} else if dataIndex >= filter.bEnd {
			// This check should ideally not be hit if loop/prepareData is correct, but keep for safety
			panic(fmt.Sprintf("calcOutputSingle: left buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}
		// If filter.bRealEnd >= 0 AND dataIndex >= filter.bRealEnd, sampleValue remains 0.0

		left += icoeff * sampleValue // Accumulate (adds 0.0 if reading padded area)
		// --- END NEW ---

		filterIndex -= increment
		dataIndex++
	}
	// fmt.Printf("[SINC_DEBUG] calcOutputSingle: Left loop executed %d times, left_accum=%.5f\n", loopCountLeft, left)

	//---------------- Apply the right half of the filter -------------------
	filterIndex = increment - startFilterIndex
	if filterIndex > maxFilterIndex {
		coeffCount = -1
	} else {
		coeffCount = int((maxFilterIndex - filterIndex) / increment)
	}
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex = filter.bCurrent + 1 + coeffCount

	right = 0.0
	// loopCountRight := 0
	for {
		// loopCountRight++
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)

		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputSingle: right coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// --- NEW Checks and Read (Right Loop) ---
		var sampleValue float64 = 0.0 // Default to 0.0 (for padded area)
		if dataIndex < 0 || dataIndex >= filter.bLen {
			panic(fmt.Sprintf("calcOutputSingle: right buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		// Only read from buffer if within valid data range (before bEnd)
		// AND within the real data range (before bRealEnd, if EOF is marked)
		if dataIndex < filter.bEnd && (filter.bRealEnd < 0 || dataIndex < filter.bRealEnd) {
			sampleValue = float64(filter.buffer[dataIndex]) // Read real data
		} else if dataIndex >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputSingle: right buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}
		// If filter.bRealEnd >= 0 AND dataIndex >= filter.bRealEnd, sampleValue remains 0.0

		right += icoeff * sampleValue // Accumulate (adds 0.0 if reading padded area)
		// --- END NEW ---

		filterIndex -= increment
		dataIndex--

		if !(filterIndex > 0) {
			break
		}
	}
	// fmt.Printf("[SINC_DEBUG] calcOutputSingle: Right loop executed %d times, right_accum=%.5f\n", loopCountRight, right)
	// fmt.Printf("[SINC_DEBUG] calcOutputSingle: EXIT - Returning %.5f\n", left+right)

	return left + right
}

// calcOutputStereo calculates a pair of interpolated stereo output samples.
// Corresponds to calc_output_stereo in src_sinc.c
func calcOutputStereo(filter *sincFilter, channels int, increment, startFilterIndex incrementT, scale float64, output []float32) {
	if len(output) < 2 {
		panic(fmt.Sprintf("calcOutputStereo: output slice too small (len=%d, need 2)", len(output)))
	}
	if channels != 2 {
		panic(fmt.Sprintf("calcOutputStereo called with incorrect channel count: %d", channels))
	}

	var left, right [2]float64
	maxFilterIndex := intToFP(filter.coeffHalfLen)

	//---------------- Apply the left half of the filter --------------------
	filterIndex := startFilterIndex
	if increment <= 0 {
		panic(fmt.Sprintf("calcOutputStereo: invalid increment %d", increment))
	}
	coeffCount := int((maxFilterIndex - filterIndex) / increment)
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex := filter.bCurrent - channels*coeffCount

	if dataIndex < 0 {
		steps := intDivCeil(-dataIndex, channels)
		maxSteps := intDivCeil(int(filterIndex), int(increment))
		if filterIndex < 0 {
			maxSteps = intDivCeil(int(-filterIndex+increment-1), int(increment))
		}
		if steps > maxSteps {
			panic(fmt.Sprintf("calcOutputStereo: buffer underflow assertion failed (steps=%d > maxSteps=%d, filterIndex=%d, increment=%d)", steps, maxSteps, filterIndex, increment))
		}
		filterIndex -= incrementT(steps) * increment
		dataIndex += steps * channels
	}

	left[0], left[1] = 0.0, 0.0
	for filterIndex >= 0 {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)
		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputStereo: left coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// --- NEW Checks and Read (Left Loop - Stereo) ---
		endDataIdx := dataIndex + 1 // Check up to the second channel
		if dataIndex < 0 || endDataIdx >= filter.bLen {
			panic(fmt.Sprintf("calcOutputStereo: left buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}

		canReadCh0 := dataIndex < filter.bEnd && (filter.bRealEnd < 0 || dataIndex < filter.bRealEnd)
		canReadCh1 := endDataIdx < filter.bEnd && (filter.bRealEnd < 0 || endDataIdx < filter.bRealEnd)

		if dataIndex >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputStereo: left buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		} // Should ideally check endDataIdx too? C only checks index+1 < b_end

		sampleValueCh0 := 0.0
		if canReadCh0 {
			sampleValueCh0 = float64(filter.buffer[dataIndex])
		}
		sampleValueCh1 := 0.0
		if canReadCh1 {
			sampleValueCh1 = float64(filter.buffer[dataIndex+1])
		}

		left[0] += icoeff * sampleValueCh0
		left[1] += icoeff * sampleValueCh1
		// --- END NEW ---

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

	right[0], right[1] = 0.0, 0.0
	for {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)
		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputStereo: right coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// --- NEW Checks and Read (Right Loop - Stereo) ---
		endDataIdx := dataIndex + 1
		if dataIndex < 0 || endDataIdx >= filter.bLen {
			panic(fmt.Sprintf("calcOutputStereo: right buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}

		canReadCh0 := dataIndex < filter.bEnd && (filter.bRealEnd < 0 || dataIndex < filter.bRealEnd)
		canReadCh1 := endDataIdx < filter.bEnd && (filter.bRealEnd < 0 || endDataIdx < filter.bRealEnd)

		if dataIndex >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputStereo: right buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}

		sampleValueCh0 := 0.0
		if canReadCh0 {
			sampleValueCh0 = float64(filter.buffer[dataIndex])
		}
		sampleValueCh1 := 0.0
		if canReadCh1 {
			sampleValueCh1 = float64(filter.buffer[dataIndex+1])
		}

		right[0] += icoeff * sampleValueCh0
		right[1] += icoeff * sampleValueCh1
		// --- END NEW ---

		filterIndex -= increment
		dataIndex -= channels

		if !(filterIndex > 0) {
			break
		}
	}

	// --- Combine, scale, and write output ---
	output[0] = float32(scale * (left[0] + right[0]))
	output[1] = float32(scale * (left[1] + right[1]))
}

// calcOutputQuad calculates a set of 4 interpolated quad output samples.
// Corresponds to calc_output_quad in src_sinc.c
func calcOutputQuad(filter *sincFilter, channels int, increment, startFilterIndex incrementT, scale float64, output []float32) {
	if len(output) < 4 {
		panic(fmt.Sprintf("calcOutputQuad: output slice too small (len=%d, need 4)", len(output)))
	}
	if channels != 4 {
		panic(fmt.Sprintf("calcOutputQuad called with incorrect channel count: %d", channels))
	}

	var left, right [4]float64
	maxFilterIndex := intToFP(filter.coeffHalfLen)

	//---------------- Apply the left half of the filter --------------------
	filterIndex := startFilterIndex
	if increment <= 0 {
		panic(fmt.Sprintf("calcOutputQuad: invalid increment %d", increment))
	}
	coeffCount := int((maxFilterIndex - filterIndex) / increment)
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex := filter.bCurrent - channels*coeffCount

	if dataIndex < 0 {
		steps := intDivCeil(-dataIndex, channels)
		maxSteps := intDivCeil(int(filterIndex), int(increment))
		if filterIndex < 0 {
			maxSteps = intDivCeil(int(-filterIndex+increment-1), int(increment))
		}
		if steps > maxSteps {
			panic(fmt.Sprintf("calcOutputQuad: buffer underflow assertion failed (steps=%d > maxSteps=%d, filterIndex=%d, increment=%d)", steps, maxSteps, filterIndex, increment))
		}
		filterIndex -= incrementT(steps) * increment
		dataIndex += steps * channels
	}

	for ch := 0; ch < 4; ch++ {
		left[ch] = 0.0
	}
	for filterIndex >= 0 {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)
		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputQuad: left coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// --- NEW Checks and Read (Left Loop - Quad) ---
		endDataIdx := dataIndex + 3 // Check up to the last channel
		if dataIndex < 0 || endDataIdx >= filter.bLen {
			panic(fmt.Sprintf("calcOutputQuad: left buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputQuad: left buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		} // C checks +3 < b_end

		for ch := 0; ch < 4; ch++ {
			sampleValue := 0.0
			checkIdx := dataIndex + ch
			if checkIdx < filter.bEnd && (filter.bRealEnd < 0 || checkIdx < filter.bRealEnd) {
				sampleValue = float64(filter.buffer[checkIdx])
			}
			left[ch] += icoeff * sampleValue
		}
		// --- END NEW ---

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

	for ch := 0; ch < 4; ch++ {
		right[ch] = 0.0
	}
	for {
		fraction := fpToDouble(filterIndex)
		indx := fpToInt(filterIndex)
		if indx < 0 || indx+1 >= len(filter.coeffs) {
			panic(fmt.Sprintf("calcOutputQuad: right coefficient index out of bounds (indx=%d, len=%d)", indx, len(filter.coeffs)))
		}
		icoeff := float64(filter.coeffs[indx]) + fraction*float64(filter.coeffs[indx+1]-filter.coeffs[indx])

		// --- NEW Checks and Read (Right Loop - Quad) ---
		endDataIdx := dataIndex + 3
		if dataIndex < 0 || endDataIdx >= filter.bLen {
			panic(fmt.Sprintf("calcOutputQuad: right buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputQuad: right buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}

		for ch := 0; ch < 4; ch++ {
			sampleValue := 0.0
			checkIdx := dataIndex + ch
			if checkIdx < filter.bEnd && (filter.bRealEnd < 0 || checkIdx < filter.bRealEnd) {
				sampleValue = float64(filter.buffer[checkIdx])
			}
			right[ch] += icoeff * sampleValue
		}
		// --- END NEW ---

		filterIndex -= increment
		dataIndex -= channels

		if !(filterIndex > 0) {
			break
		}
	}

	// --- Combine, scale, and write output ---
	for ch := 0; ch < 4; ch++ {
		output[ch] = float32(scale * (left[ch] + right[ch]))
	}
}

// calcOutputHex calculates a set of 6 interpolated hex output samples.
// Corresponds to calc_output_hex in src_sinc.c
func calcOutputHex(filter *sincFilter, channels int, increment, startFilterIndex incrementT, scale float64, output []float32) {
	if len(output) < 6 {
		panic(fmt.Sprintf("calcOutputHex: output slice too small (len=%d, need 6)", len(output)))
	}
	if channels != 6 {
		panic(fmt.Sprintf("calcOutputHex called with incorrect channel count: %d", channels))
	}

	var left, right [6]float64
	maxFilterIndex := intToFP(filter.coeffHalfLen)

	//---------------- Apply the left half of the filter --------------------
	filterIndex := startFilterIndex
	if increment <= 0 {
		panic(fmt.Sprintf("calcOutputHex: invalid increment %d", increment))
	}
	coeffCount := int((maxFilterIndex - filterIndex) / increment)
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex := filter.bCurrent - channels*coeffCount

	if dataIndex < 0 {
		steps := intDivCeil(-dataIndex, channels)
		maxSteps := intDivCeil(int(filterIndex), int(increment))
		if filterIndex < 0 {
			maxSteps = intDivCeil(int(-filterIndex+increment-1), int(increment))
		}
		if steps > maxSteps {
			panic(fmt.Sprintf("calcOutputHex: buffer underflow assertion failed (steps=%d > maxSteps=%d, filterIndex=%d, increment=%d)", steps, maxSteps, filterIndex, increment))
		}
		filterIndex -= incrementT(steps) * increment
		dataIndex += steps * channels
	}

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

		// --- NEW Checks and Read (Left Loop - Hex) ---
		endDataIdx := dataIndex + 5
		if dataIndex < 0 || endDataIdx >= filter.bLen {
			panic(fmt.Sprintf("calcOutputHex: left buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputHex: left buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}

		for ch := 0; ch < 6; ch++ {
			sampleValue := 0.0
			checkIdx := dataIndex + ch
			if checkIdx < filter.bEnd && (filter.bRealEnd < 0 || checkIdx < filter.bRealEnd) {
				sampleValue = float64(filter.buffer[checkIdx])
			}
			left[ch] += icoeff * sampleValue
		}
		// --- END NEW ---

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

		// --- NEW Checks and Read (Right Loop - Hex) ---
		endDataIdx := dataIndex + 5
		if dataIndex < 0 || endDataIdx >= filter.bLen {
			panic(fmt.Sprintf("calcOutputHex: right buffer index out of allocated bounds (dataIndex=%d, bLen=%d)", dataIndex, filter.bLen))
		}
		if dataIndex >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputHex: right buffer index out of valid data range (dataIndex=%d, bEnd=%d)", dataIndex, filter.bEnd))
		}

		for ch := 0; ch < 6; ch++ {
			sampleValue := 0.0
			checkIdx := dataIndex + ch
			if checkIdx < filter.bEnd && (filter.bRealEnd < 0 || checkIdx < filter.bRealEnd) {
				sampleValue = float64(filter.buffer[checkIdx])
			}
			right[ch] += icoeff * sampleValue
		}
		// --- END NEW ---

		filterIndex -= increment
		dataIndex -= channels

		if !(filterIndex > 0) {
			break
		}
	}

	// --- Combine, scale, and write output ---
	for ch := 0; ch < 6; ch++ {
		output[ch] = float32(scale * (left[ch] + right[ch]))
	}
}

// calcOutputMulti calculates a set of interpolated output samples for multiple channels.
// Corresponds to calc_output_multi in src_sinc.c (Now Implemented)
func calcOutputMulti(filter *sincFilter, channels int, increment, startFilterIndex incrementT, scale float64, output []float32) {
	if len(output) < channels {
		panic(fmt.Sprintf("calcOutputMulti: output slice too small (len=%d, need %d)", len(output), channels))
	}
	if channels > maxChannels {
		panic(fmt.Sprintf("calcOutputMulti: channel count %d exceeds maxChannels %d", channels, maxChannels))
	}

	left := filter.leftCalc[:channels]
	right := filter.rightCalc[:channels]

	for ch := 0; ch < channels; ch++ {
		left[ch], right[ch] = 0.0, 0.0
	}

	maxFilterIndex := intToFP(filter.coeffHalfLen)

	//---------------- Apply the left half of the filter --------------------
	filterIndex := startFilterIndex
	if increment <= 0 {
		panic(fmt.Sprintf("calcOutputMulti: invalid increment %d", increment))
	}
	coeffCount := int((maxFilterIndex - filterIndex) / increment)
	filterIndex = filterIndex + incrementT(coeffCount)*increment
	dataIndex := filter.bCurrent - channels*coeffCount

	if dataIndex < 0 {
		steps := intDivCeil(-dataIndex, channels)
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

		// --- NEW Checks and Read (Left Loop - Multi) ---
		endDataIdx := dataIndex + channels - 1
		if dataIndex < 0 || endDataIdx >= filter.bLen {
			panic(fmt.Sprintf("calcOutputMulti: left buffer index out of allocated bounds (dataIndex=%d, channels=%d, bLen=%d)", dataIndex, channels, filter.bLen))
		}
		if dataIndex >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputMulti: left buffer index out of valid data range (dataIndex=%d, channels=%d, bEnd=%d)", dataIndex, channels, filter.bEnd))
		} // C checks +channels-1 < b_end

		for ch := 0; ch < channels; ch++ {
			sampleValue := 0.0
			checkIdx := dataIndex + ch
			// Only read if index is within BOTH bEnd and bRealEnd (if set)
			if checkIdx < filter.bEnd && (filter.bRealEnd < 0 || checkIdx < filter.bRealEnd) {
				sampleValue = float64(filter.buffer[checkIdx])
			}
			left[ch] += icoeff * sampleValue
		}
		// --- END NEW ---

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

		// --- NEW Checks and Read (Right Loop - Multi) ---
		endDataIdx := dataIndex + channels - 1
		if dataIndex < 0 || endDataIdx >= filter.bLen {
			panic(fmt.Sprintf("calcOutputMulti: right buffer index out of allocated bounds (dataIndex=%d, channels=%d, bLen=%d)", dataIndex, channels, filter.bLen))
		}
		if dataIndex >= filter.bEnd {
			panic(fmt.Sprintf("calcOutputMulti: right buffer index out of valid data range (dataIndex=%d, channels=%d, bEnd=%d)", dataIndex, channels, filter.bEnd))
		}

		for ch := 0; ch < channels; ch++ {
			sampleValue := 0.0
			checkIdx := dataIndex + ch
			if checkIdx < filter.bEnd && (filter.bRealEnd < 0 || checkIdx < filter.bRealEnd) {
				sampleValue = float64(filter.buffer[checkIdx])
			}
			right[ch] += icoeff * sampleValue
		}
		// --- END NEW ---

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

// --- TODO: Implement Processing & Helper Functions ---

// sincMonoVariProcess handles mono data with potentially varying sample rate ratio.
// Corresponds to sinc_mono_vari_process in src_sinc.c (Corrected version)
func sincMonoVariProcess(state *srcState, data *SrcData) ErrorCode {
	if sincDebugEnabled {
		fmt.Printf("\n[SINC_DEBUG] sincMonoVariProcess: ENTRY - data.InFrames=%d, data.OutFrames=%d, data.SrcRatio=%.5f, data.EOF=%t\n",
			data.InputFrames, data.OutputFrames, data.SrcRatio, data.EndOfInput)
		fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: State - lastRatio=%.5f, lastPos=%.5f\n", state.lastRatio, state.lastPosition)
	}

	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: ERROR: Invalid private data.\n")
		}
		return ErrBadState
	}
	if state.channels != 1 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: ERROR: Incorrect channel count (%d).\n", state.channels)
		}
		return ErrBadInternalState
	}
	inputIndex := state.lastPosition
	srcRatio := state.lastRatio
	var increment, startFilterIndex incrementT
	var halfFilterChanLen, samplesInHand int
	outCountSamples := data.OutputFrames * int64(state.channels)
	data.InputFramesUsed = 0    // Reset before processing
	data.OutputFramesGen = 0    // Reset before processing
	var inUsedSamples int64 = 0 // Tracks samples consumed *by prepareData* cumulatively in this call
	var outGenSamples int64 = 0 // Tracks samples generated *in this call*

	// Initialize srcRatio if needed (first call)
	if isBadSrcRatio(srcRatio) {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: Initializing srcRatio from data.SrcRatio (%.5f)\n", data.SrcRatio)
		}
		if isBadSrcRatio(data.SrcRatio) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: ERROR: Bad initial srcRatio from data.\n")
			}
			return ErrBadSrcRatio
		}
		srcRatio = data.SrcRatio
		// state.lastRatio = srcRatio // Don't update state.lastRatio yet, use local srcRatio for consistency check below
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: Effective srcRatio for start = %.5f\n", srcRatio)
	}

	// Calculate required lookback/lookahead based on filter length and minimum ratio
	filterCoeffsLen := float64(filter.coeffHalfLen + 2)
	if filter.indexInc <= 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: ERROR: Bad filter.indexInc (%d).\n", filter.indexInc)
		}
		return ErrBadInternalState
	}
	count := filterCoeffsLen / float64(filter.indexInc)
	//minRatio := minFloat64(state.lastRatio, data.SrcRatio) // Use state.lastRatio here? C uses local src_ratio, which might be data->src_ratio initially. Let's use data->SrcRatio if state.lastRatio is invalid.
	effectiveMinRatio := srcRatio        // Start with current effective ratio
	if !isBadSrcRatio(state.lastRatio) { // If lastRatio was valid
		effectiveMinRatio = minFloat64(state.lastRatio, srcRatio) // Consider variation
	}
	if effectiveMinRatio < (1.0 / srcMaxRatio) {
		effectiveMinRatio = 1.0 / srcMaxRatio
	}
	if effectiveMinRatio < 1.0 && effectiveMinRatio > 1e-10 { // Avoid division by zero/very small
		count /= effectiveMinRatio
	} else if effectiveMinRatio <= 1e-10 {
		// Handle extremely small ratio case - required lookback could be huge. Cap it?
		// Let's just use a large multiplier for count to be safe, matching C's implicit large result.
		count *= srcMaxRatio // Arbitrary large factor if ratio is near zero
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: WARNING: Very small minRatio (%.5f), using large lookback factor.\n", effectiveMinRatio)
		}
	}

	halfFilterChanLen = state.channels * (psfLrint(count) + 1)
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: Calculated halfFilterChanLen = %d\n", halfFilterChanLen)
	}

	// Advance internal buffer pointer based on integer part of inputIndex
	intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex))
	if filter.bLen <= 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: ERROR: Bad filter.bLen (%d).\n", filter.bLen)
		}
		return ErrBadInternalState
	}
	// Wrap bCurrent using modulo
	newBCurrent := (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
	if newBCurrent < 0 { // Ensure positive result from modulo
		newBCurrent += filter.bLen
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: Advancing bCurrent by %d samples from %d to %d (modulo %d).\n", state.channels*intInputAdvance, filter.bCurrent, newBCurrent, filter.bLen)
	}
	filter.bCurrent = newBCurrent
	inputIndex = fmodOne(inputIndex) // Keep only fractional part

	// Main processing loop
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: Starting main loop. Target output samples = %d\n", outCountSamples)
	}
	for outGenSamples < outCountSamples {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: Loop Iteration %d. outGenSamples=%d\n", outGenSamples, outGenSamples)
		}

		// Calculate samples currently available in the buffer
		if filter.bEnd >= filter.bCurrent {
			samplesInHand = filter.bEnd - filter.bCurrent
		} else {
			samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
		}
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: samplesInHand=%d (bEnd=%d, bCurrent=%d, bLen=%d). Needed=%d\n", samplesInHand, filter.bEnd, filter.bCurrent, filter.bLen, halfFilterChanLen)
		}

		// Check if we need more data (including lookback/lookahead)
		if samplesInHand <= halfFilterChanLen {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: samplesInHand <= halfFilterChanLen. Calling prepareData.\n")
			}
			// Need to update data.InputFramesUsed *before* calling prepareData if it was modified internally
			data.InputFramesUsed = inUsedSamples / int64(state.channels)

			errCode := prepareData(filter, state.channels, data, halfFilterChanLen)
			if errCode != ErrNoError {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: prepareData returned error: %d\n", errCode)
				}
				state.errCode = errCode
				return errCode // Propagate error
			}
			// Update local counter of used samples based on what prepareData consumed
			inUsedSamples = data.InputFramesUsed * int64(state.channels)

			// Recalculate samplesInHand after prepareData
			if filter.bEnd >= filter.bCurrent {
				samplesInHand = filter.bEnd - filter.bCurrent
			} else {
				samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
			}
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: After prepareData: samplesInHand=%d, inUsedSamples=%d (data.InputFramesUsed=%d)\n", samplesInHand, inUsedSamples, data.InputFramesUsed)
			}

			// If still not enough samples after trying to prepare, we must break (EOF or insufficient buffer)
			if samplesInHand <= halfFilterChanLen {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: samplesInHand *still* <= halfFilterChanLen (%d <= %d). Breaking loop.\n", samplesInHand, halfFilterChanLen)
				}
				break // Exit the loop
			}
		}

		// Check for End Of Input condition within the buffer
		// If bRealEnd is set (by prepareData padding) and we need data beyond it, stop.
		//if filter.bRealEnd >= 0 {
		//	// Calculate the furthest index needed for the current output sample calculation
		//	maxIndexNeeded := filter.bCurrent + halfFilterChanLen // Approximate index needed
		//	// Note: This check is approximate. The actual max index depends on startFilterIndex/increment.
		//	// C code just checks `b_current + half_filter_chan_len`.
		//	// A more accurate check might involve the dataIndex calculation within calcOutput*.
		//	// Let's stick to the C check for now.
		//	fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: EOF Check: bRealEnd=%d, maxIndexNeeded (approx)=%d\n", filter.bRealEnd, maxIndexNeeded)
		//	if maxIndexNeeded >= filter.bRealEnd {
		//		fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: maxIndexNeeded >= bRealEnd. Breaking loop due to EOF.\n")
		//		break                                                                                                   // Cannot calculate more output, reached end of real data + padding
		//	}
		//}
		if filter.bRealEnd >= 0 {
			// Calculate approximate position corresponding to the *next* output sample's center
			// C uses 'terminate' which is approx 1.0 / src_ratio
			// C checks: b_current + input_index (fractional) + terminate
			// Need to use the *current* effective srcRatio for terminate calculation
			terminate := 1.0/srcRatio + 1e-20                                  // Use current loop's srcRatio
			checkPosition := float64(filter.bCurrent) + inputIndex + terminate // Approximate position needed for next sample

			// C Check: if (checkPosition >= filter->b_real_end) break;
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] ... EOF Check: bRealEnd=%d, checkPosition(curr+idx+1/ratio)=%.2f\n", filter.bRealEnd, checkPosition)
			}
			if checkPosition >= float64(filter.bRealEnd) {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] ... Breaking loop due to EOF check (C logic).\n")
				}
				break // Break loop if EOF reached
			}
		}
		// Vary ratio if needed (only if target output count > 0)
		if outCountSamples > 0 && math.Abs(state.lastRatio-data.SrcRatio) > srcMinRatioDiff {
			srcRatio = state.lastRatio + float64(outGenSamples)*(data.SrcRatio-state.lastRatio)/float64(outCountSamples)
			if isBadSrcRatio(srcRatio) {
				// Clip ratio if it goes out of bounds during variation
				if srcRatio < 1.0/srcMaxRatio {
					srcRatio = 1.0 / srcMaxRatio
				}
				if srcRatio > srcMaxRatio {
					srcRatio = srcMaxRatio
				}
			}
			// fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: Varied srcRatio to %.5f\n", srcRatio)
		}

		// Calculate fixed point increment based on potentially varying srcRatio
		// C uses min (src_ratio, 1.0) - this ensures increment doesn't exceed indexInc when upsampling
		floatIncrement := float64(filter.indexInc) * minFloat64(srcRatio, 1.0)
		increment = doubleToFP(floatIncrement)
		if increment == 0 {
			// This happens if srcRatio is extremely small, making floatIncrement effectively zero.
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: ERROR: Calculated increment is zero (srcRatio=%.15f, floatInc=%.15f).\n", srcRatio, floatIncrement)
			}
			// What should happen here? C returns error.
			state.errCode = ErrBadSrcRatio // Or maybe ErrBadInternalState?
			return state.errCode
		}

		// Calculate start index for filter coefficients based on fractional input position
		// startFilterIndex = inputIndex * increment // This seems wrong, C uses inputIndex * floatIncrement
		startFilterIndex = doubleToFP(inputIndex * floatIncrement)

		// Calculate scaling factor for output sample
		// scaleFactor = increment / filter.indexInc ; C uses float_increment
		scaleFactor := floatIncrement / float64(filter.indexInc)

		// fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: Calculating output: increment=%d, startFilterIndex=%d, scaleFactor=%.5f\n", increment, startFilterIndex, scaleFactor)

		// Calculate the output sample
		// Ensure calcOutputSingle handles boundary conditions robustly
		outputSample := scaleFactor * calcOutputSingle(filter, increment, startFilterIndex)

		// Store the output sample
		outPos := int(outGenSamples)
		if outPos >= len(data.DataOut) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: WARNING: Output buffer full (outPos=%d, len=%d). Breaking loop.\n", outPos, len(data.DataOut))
			}
			// This indicates the provided output buffer was too small for the requested OutputFrames
			state.errCode = ErrNoError // Not necessarily an error state, just can't write more
			break                      // Exit the loop
		}
		data.DataOut[outPos] = float32(outputSample)
		outGenSamples++

		// fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: Generated sample %d = %.5f\n", outGenSamples, outputSample)

		// Update input index position for the next output sample
		if srcRatio <= 1e-10 { // Avoid division by zero/very small
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: ERROR: srcRatio is zero or very small (%.15f), cannot advance input index.\n", srcRatio)
			}
			state.errCode = ErrBadSrcRatio
			return state.errCode
		}
		inputIndex += 1.0 / srcRatio

		// Advance internal buffer pointer based on integer part of new inputIndex
		intInputAdvance = psfLrint(inputIndex - fmodOne(inputIndex))
		newBCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
		if newBCurrent < 0 {
			newBCurrent += filter.bLen
		}
		// fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: Advanced inputIndex to %.5f, intInputAdvance=%d, moving bCurrent from %d to %d.\n", inputIndex, intInputAdvance, filter.bCurrent, newBCurrent)
		filter.bCurrent = newBCurrent
		inputIndex = fmodOne(inputIndex) // Keep only fractional part
	} // End main processing loop

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: Exited main loop.\n")
	}

	// Store final state
	state.lastPosition = inputIndex
	state.lastRatio = srcRatio // Store the potentially varied ratio used for the *last* sample
	data.OutputFramesGen = outGenSamples / int64(state.channels)
	// Ensure InputFramesUsed reflects *all* consumption triggered by prepareData calls
	data.InputFramesUsed = inUsedSamples / int64(state.channels)

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMonoVariProcess: EXIT - data.OutGen=%d, data.InUsed=%d, state.lastPos=%.5f\n",
			data.OutputFramesGen, data.InputFramesUsed, state.lastPosition)
	}

	// If no error occurred within the loop, return success
	if state.errCode == ErrNoError {
		return ErrNoError
	}
	// Otherwise, return the error code that was set
	return state.errCode
}

// sincStereoVariProcess handles stereo data with potentially varying sample rate ratio.
// Corresponds to sinc_stereo_vari_process in src_sinc.c
func sincStereoVariProcess(state *srcState, data *SrcData) ErrorCode {
	if sincDebugEnabled {
		fmt.Printf("\n[SINC_DEBUG] sincStereoVariProcess: ENTRY - data.InFrames=%d, data.OutFrames=%d, data.SrcRatio=%.5f, data.EOF=%t\n",
			data.InputFrames, data.OutputFrames, data.SrcRatio, data.EndOfInput)
		fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: State - lastRatio=%.5f, lastPos=%.5f\n", state.lastRatio, state.lastPosition)
	}

	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: ERROR: Invalid private data.\n")
		}
		return ErrBadState
	}
	if state.channels != 2 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: ERROR: Incorrect channel count (%d).\n", state.channels)
		}
		return ErrBadInternalState
	}
	inputIndex := state.lastPosition
	srcRatio := state.lastRatio
	var increment, startFilterIndex incrementT
	var halfFilterChanLen, samplesInHand int
	outCountSamples := data.OutputFrames * int64(state.channels) // Total samples to generate
	data.InputFramesUsed = 0                                     // Reset before processing
	data.OutputFramesGen = 0                                     // Reset before processing
	var inUsedSamples int64 = 0                                  // Tracks samples consumed by prepareData
	var outGenSamples int64 = 0                                  // Tracks samples generated

	// Initialize srcRatio if needed
	if isBadSrcRatio(srcRatio) {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: Initializing srcRatio from data.SrcRatio (%.5f)\n", data.SrcRatio)
		}
		if isBadSrcRatio(data.SrcRatio) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: ERROR: Bad initial srcRatio from data.\n")
			}
			return ErrBadSrcRatio
		}
		srcRatio = data.SrcRatio
		// state.lastRatio = srcRatio // Use local srcRatio for consistency check below
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: Effective srcRatio for start = %.5f\n", srcRatio)
	}

	// Calculate required lookback/lookahead
	filterCoeffsLen := float64(filter.coeffHalfLen + 2)
	if filter.indexInc <= 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: ERROR: Bad filter.indexInc (%d).\n", filter.indexInc)
		}
		return ErrBadInternalState
	}
	count := filterCoeffsLen / float64(filter.indexInc)
	effectiveMinRatio := srcRatio
	if !isBadSrcRatio(state.lastRatio) {
		effectiveMinRatio = minFloat64(state.lastRatio, srcRatio)
	}
	if effectiveMinRatio < (1.0 / srcMaxRatio) {
		effectiveMinRatio = 1.0 / srcMaxRatio
	}
	if effectiveMinRatio < 1.0 && effectiveMinRatio > 1e-10 {
		count /= effectiveMinRatio
	} else if effectiveMinRatio <= 1e-10 {
		count *= srcMaxRatio
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: WARNING: Very small minRatio (%.5f), using large lookback factor.\n", effectiveMinRatio)
		}
	}
	halfFilterChanLen = state.channels * (psfLrint(count) + 1)
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: Calculated halfFilterChanLen = %d\n", halfFilterChanLen)
	}

	// Advance internal buffer pointer
	intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex))
	if filter.bLen <= 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: ERROR: Bad filter.bLen (%d).\n", filter.bLen)
		}
		return ErrBadInternalState
	}
	newBCurrent := (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
	if newBCurrent < 0 {
		newBCurrent += filter.bLen
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: Advancing bCurrent by %d samples from %d to %d (modulo %d).\n", state.channels*intInputAdvance, filter.bCurrent, newBCurrent, filter.bLen)
	}
	filter.bCurrent = newBCurrent
	inputIndex = fmodOne(inputIndex)

	// Main processing loop
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: Starting main loop. Target output samples = %d\n", outCountSamples)
	}
	for outGenSamples < outCountSamples {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: Loop Iteration %d. outGenSamples=%d\n", outGenSamples/int64(state.channels), outGenSamples)
		}

		// Calculate samples available
		if filter.bEnd >= filter.bCurrent {
			samplesInHand = filter.bEnd - filter.bCurrent
		} else {
			samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
		}
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: samplesInHand=%d. Needed=%d\n", samplesInHand, halfFilterChanLen)
		}

		// Need more data?
		if samplesInHand <= halfFilterChanLen {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: samplesInHand <= halfFilterChanLen. Calling prepareData.\n")
			}
			data.InputFramesUsed = inUsedSamples / int64(state.channels) // Update before call
			errCode := prepareData(filter, state.channels, data, halfFilterChanLen)
			if errCode != ErrNoError {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: prepareData returned error: %d\n", errCode)
				}
				state.errCode = errCode
				return errCode
			}
			inUsedSamples = data.InputFramesUsed * int64(state.channels) // Update after call

			// Recalculate samplesInHand
			if filter.bEnd >= filter.bCurrent {
				samplesInHand = filter.bEnd - filter.bCurrent
			} else {
				samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
			}
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: After prepareData: samplesInHand=%d, inUsedSamples=%d (data.InputFramesUsed=%d)\n", samplesInHand, inUsedSamples, data.InputFramesUsed)
			}

			// Break if still not enough
			if samplesInHand <= halfFilterChanLen {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: samplesInHand *still* <= halfFilterChanLen (%d <= %d). Breaking loop.\n", samplesInHand, halfFilterChanLen)
				}
				break
			}
		}

		// Check EOF condition
		if filter.bRealEnd >= 0 {
			terminate := 1.0/srcRatio + 1e-20                                  // Use current loop's srcRatio
			checkPosition := float64(filter.bCurrent) + inputIndex + terminate // Approximate position needed for next sample

			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] ... EOF Check: bRealEnd=%d, checkPosition(curr+idx+1/ratio)=%.2f\n", filter.bRealEnd, checkPosition)
			}
			if checkPosition >= float64(filter.bRealEnd) {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] ... Breaking loop due to EOF check (C logic).\n")
				}
				break // Break loop if EOF reached
			}
		}
		// Vary ratio if needed
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

		// Calculate parameters for calcOutputStereo
		floatIncrement := float64(filter.indexInc) * minFloat64(srcRatio, 1.0)
		increment = doubleToFP(floatIncrement)
		if increment == 0 {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: ERROR: Calculated increment is zero (srcRatio=%.15f, floatInc=%.15f).\n", srcRatio, floatIncrement)
			}
			state.errCode = ErrBadSrcRatio
			return state.errCode
		}
		startFilterIndex = doubleToFP(inputIndex * floatIncrement)
		scaleFactor := floatIncrement / float64(filter.indexInc)

		// Get output slice
		outPos := int(outGenSamples)
		if outPos+state.channels > len(data.DataOut) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: WARNING: Output buffer full (outPos=%d, channels=%d, len=%d). Breaking loop.\n", outPos, state.channels, len(data.DataOut))
			}
			break
		}
		outputSlice := data.DataOut[outPos : outPos+state.channels]

		// Calculate the output frame (stereo pair)
		calcOutputStereo(filter, state.channels, increment, startFilterIndex, scaleFactor, outputSlice)
		outGenSamples += int64(state.channels) // Increment by number of channels

		// fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: Generated frame %d = [%.5f, %.5f]\n", outGenSamples/int64(state.channels), outputSlice[0], outputSlice[1])

		// Update input index
		if srcRatio <= 1e-10 {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: ERROR: srcRatio is zero or very small (%.15f), cannot advance input index.\n", srcRatio)
			}
			state.errCode = ErrBadSrcRatio
			return state.errCode
		}
		inputIndex += 1.0 / srcRatio

		// Advance buffer pointer
		intInputAdvance = psfLrint(inputIndex - fmodOne(inputIndex))
		newBCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
		if newBCurrent < 0 {
			newBCurrent += filter.bLen
		}
		// fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: Advanced inputIndex to %.5f, intInputAdvance=%d, moving bCurrent from %d to %d.\n", inputIndex, intInputAdvance, filter.bCurrent, newBCurrent)
		filter.bCurrent = newBCurrent
		inputIndex = fmodOne(inputIndex)

	} // End main processing loop

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: Exited main loop.\n")
	}

	// Store final state
	state.lastPosition = inputIndex
	state.lastRatio = srcRatio
	data.OutputFramesGen = outGenSamples / int64(state.channels)
	data.InputFramesUsed = inUsedSamples / int64(state.channels) // Ensure this reflects total consumed

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincStereoVariProcess: EXIT - data.OutGen=%d, data.InUsed=%d, state.lastPos=%.5f\n", data.OutputFramesGen, data.InputFramesUsed, state.lastPosition)
	}

	if state.errCode == ErrNoError {
		return ErrNoError
	}
	return state.errCode
}

// sincQuadVariProcess handles quad audio data with potentially varying sample rate ratio.
// Corresponds to sinc_quad_vari_process in src_sinc.c
func sincQuadVariProcess(state *srcState, data *SrcData) ErrorCode {
	if sincDebugEnabled {
		fmt.Printf("\n[SINC_DEBUG] sincQuadVariProcess: ENTRY - data.InFrames=%d, data.OutFrames=%d, data.SrcRatio=%.5f, data.EOF=%t\n",
			data.InputFrames, data.OutputFrames, data.SrcRatio, data.EndOfInput)
		fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: State - lastRatio=%.5f, lastPos=%.5f\n", state.lastRatio, state.lastPosition)
	}

	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: ERROR: Invalid private data.\n")
		}
		return ErrBadState
	}
	if state.channels != 4 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: ERROR: Incorrect channel count (%d).\n", state.channels)
		}
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

	// Init ratio
	if isBadSrcRatio(srcRatio) {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: Initializing srcRatio from data.SrcRatio (%.5f)\n", data.SrcRatio)
		}
		if isBadSrcRatio(data.SrcRatio) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: ERROR: Bad initial srcRatio from data.\n")
			}
			return ErrBadSrcRatio
		}
		srcRatio = data.SrcRatio
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: Effective srcRatio for start = %.5f\n", srcRatio)
	}

	// Calc lookback/ahead
	filterCoeffsLen := float64(filter.coeffHalfLen + 2)
	if filter.indexInc <= 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: ERROR: Bad filter.indexInc (%d).\n", filter.indexInc)
		}
		return ErrBadInternalState
	}
	count := filterCoeffsLen / float64(filter.indexInc)
	effectiveMinRatio := srcRatio
	if !isBadSrcRatio(state.lastRatio) {
		effectiveMinRatio = minFloat64(state.lastRatio, srcRatio)
	}
	if effectiveMinRatio < (1.0 / srcMaxRatio) {
		effectiveMinRatio = 1.0 / srcMaxRatio
	}
	if effectiveMinRatio < 1.0 && effectiveMinRatio > 1e-10 {
		count /= effectiveMinRatio
	} else if effectiveMinRatio <= 1e-10 {
		count *= srcMaxRatio
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: WARNING: Very small minRatio (%.5f), using large lookback factor.\n", effectiveMinRatio)
		}
	}
	halfFilterChanLen = state.channels * (psfLrint(count) + 1)
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: Calculated halfFilterChanLen = %d\n", halfFilterChanLen)
	}

	// Advance buffer ptr
	intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex))
	if filter.bLen <= 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: ERROR: Bad filter.bLen (%d).\n", filter.bLen)
		}
		return ErrBadInternalState
	}
	newBCurrent := (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
	if newBCurrent < 0 {
		newBCurrent += filter.bLen
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: Advancing bCurrent by %d samples from %d to %d (modulo %d).\n", state.channels*intInputAdvance, filter.bCurrent, newBCurrent, filter.bLen)
	}
	filter.bCurrent = newBCurrent
	inputIndex = fmodOne(inputIndex)

	// Main loop
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: Starting main loop. Target output samples = %d\n", outCountSamples)
	}
	for outGenSamples < outCountSamples {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: Loop Iteration %d. outGenSamples=%d\n", outGenSamples/int64(state.channels), outGenSamples)
		}

		// Samples available
		if filter.bEnd >= filter.bCurrent {
			samplesInHand = filter.bEnd - filter.bCurrent
		} else {
			samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
		}
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: samplesInHand=%d. Needed=%d\n", samplesInHand, halfFilterChanLen)
		}

		// Need more?
		if samplesInHand <= halfFilterChanLen {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: samplesInHand <= halfFilterChanLen. Calling prepareData.\n")
			}
			data.InputFramesUsed = inUsedSamples / int64(state.channels)
			errCode := prepareData(filter, state.channels, data, halfFilterChanLen)
			if errCode != ErrNoError {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: prepareData returned error: %d\n", errCode)
				}
				state.errCode = errCode
				return errCode
			}
			inUsedSamples = data.InputFramesUsed * int64(state.channels)
			if filter.bEnd >= filter.bCurrent {
				samplesInHand = filter.bEnd - filter.bCurrent
			} else {
				samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
			}
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: After prepareData: samplesInHand=%d, inUsedSamples=%d (data.InputFramesUsed=%d)\n", samplesInHand, inUsedSamples, data.InputFramesUsed)
			}
			if samplesInHand <= halfFilterChanLen {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: samplesInHand *still* <= halfFilterChanLen (%d <= %d). Breaking loop.\n", samplesInHand, halfFilterChanLen)
				}
				break
			}
		}

		// Check EOF
		if filter.bRealEnd >= 0 {
			terminate := 1.0/srcRatio + 1e-20                                  // Use current loop's srcRatio
			checkPosition := float64(filter.bCurrent) + inputIndex + terminate // Approximate position needed for next sample

			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] ... EOF Check: bRealEnd=%d, checkPosition(curr+idx+1/ratio)=%.2f\n", filter.bRealEnd, checkPosition)
			}
			if checkPosition >= float64(filter.bRealEnd) {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] ... Breaking loop due to EOF check (C logic).\n")
				}
				break // Break loop if EOF reached
			}
		}
		// Vary ratio
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

		// Calc params
		floatIncrement := float64(filter.indexInc) * minFloat64(srcRatio, 1.0)
		increment = doubleToFP(floatIncrement)
		if increment == 0 {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: ERROR: Calculated increment is zero (srcRatio=%.15f, floatInc=%.15f).\n", srcRatio, floatIncrement)
			}
			state.errCode = ErrBadSrcRatio
			return state.errCode
		}
		startFilterIndex = doubleToFP(inputIndex * floatIncrement)
		scaleFactor := floatIncrement / float64(filter.indexInc)

		// Get output slice
		outPos := int(outGenSamples)
		if outPos+state.channels > len(data.DataOut) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: WARNING: Output buffer full (outPos=%d, channels=%d, len=%d). Breaking loop.\n", outPos, state.channels, len(data.DataOut))
			}
			break
		}
		outputSlice := data.DataOut[outPos : outPos+state.channels]

		// Calc output frame
		calcOutputQuad(filter, state.channels, increment, startFilterIndex, scaleFactor, outputSlice)
		outGenSamples += int64(state.channels)

		// Update input index
		if srcRatio <= 1e-10 {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: ERROR: srcRatio is zero or very small (%.15f), cannot advance input index.\n", srcRatio)
			}
			state.errCode = ErrBadSrcRatio
			return state.errCode
		}
		inputIndex += 1.0 / srcRatio

		// Advance buffer pointer
		intInputAdvance = psfLrint(inputIndex - fmodOne(inputIndex))
		newBCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
		if newBCurrent < 0 {
			newBCurrent += filter.bLen
		}
		filter.bCurrent = newBCurrent
		inputIndex = fmodOne(inputIndex)

	} // End main loop

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: Exited main loop.\n")
	}

	// Store final state
	state.lastPosition = inputIndex
	state.lastRatio = srcRatio
	data.OutputFramesGen = outGenSamples / int64(state.channels)
	data.InputFramesUsed = inUsedSamples / int64(state.channels)

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincQuadVariProcess: EXIT - data.OutGen=%d, data.InUsed=%d, state.lastPos=%.5f\n", data.OutputFramesGen, data.InputFramesUsed, state.lastPosition)
	}

	if state.errCode == ErrNoError {
		return ErrNoError
	}
	return state.errCode
}

// sincHexVariProcess handles 6-channel audio data with potentially varying sample rate ratio.
// Corresponds to sinc_hex_vari_process in src_sinc.c
func sincHexVariProcess(state *srcState, data *SrcData) ErrorCode {
	if sincDebugEnabled {
		fmt.Printf("\n[SINC_DEBUG] sincHexVariProcess: ENTRY - data.InFrames=%d, data.OutFrames=%d, data.SrcRatio=%.5f, data.EOF=%t\n",
			data.InputFrames, data.OutputFrames, data.SrcRatio, data.EndOfInput)
		fmt.Printf("[SINC_DEBUG] sincHexVariProcess: State - lastRatio=%.5f, lastPos=%.5f\n", state.lastRatio, state.lastPosition)
	}

	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincHexVariProcess: ERROR: Invalid private data.\n")
		}
		return ErrBadState
	}
	if state.channels != 6 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincHexVariProcess: ERROR: Incorrect channel count (%d).\n", state.channels)
		}
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

	// Init ratio
	if isBadSrcRatio(srcRatio) {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincHexVariProcess: Initializing srcRatio from data.SrcRatio (%.5f)\n", data.SrcRatio)
		}
		if isBadSrcRatio(data.SrcRatio) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincHexVariProcess: ERROR: Bad initial srcRatio from data.\n")
			}
			return ErrBadSrcRatio
		}
		srcRatio = data.SrcRatio
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincHexVariProcess: Effective srcRatio for start = %.5f\n", srcRatio)
	}

	// Calc lookback/ahead
	filterCoeffsLen := float64(filter.coeffHalfLen + 2)
	if filter.indexInc <= 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincHexVariProcess: ERROR: Bad filter.indexInc (%d).\n", filter.indexInc)
		}
		return ErrBadInternalState
	}
	count := filterCoeffsLen / float64(filter.indexInc)
	effectiveMinRatio := srcRatio
	if !isBadSrcRatio(state.lastRatio) {
		effectiveMinRatio = minFloat64(state.lastRatio, srcRatio)
	}
	if effectiveMinRatio < (1.0 / srcMaxRatio) {
		effectiveMinRatio = 1.0 / srcMaxRatio
	}
	if effectiveMinRatio < 1.0 && effectiveMinRatio > 1e-10 {
		count /= effectiveMinRatio
	} else if effectiveMinRatio <= 1e-10 {
		count *= srcMaxRatio
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincHexVariProcess: WARNING: Very small minRatio (%.5f), using large lookback factor.\n", effectiveMinRatio)
		}
	}
	halfFilterChanLen = state.channels * (psfLrint(count) + 1)
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincHexVariProcess: Calculated halfFilterChanLen = %d\n", halfFilterChanLen)
	}

	// Advance buffer ptr
	intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex))
	if filter.bLen <= 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincHexVariProcess: ERROR: Bad filter.bLen (%d).\n", filter.bLen)
		}
		return ErrBadInternalState
	}
	newBCurrent := (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
	if newBCurrent < 0 {
		newBCurrent += filter.bLen
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincHexVariProcess: Advancing bCurrent by %d samples from %d to %d (modulo %d).\n", state.channels*intInputAdvance, filter.bCurrent, newBCurrent, filter.bLen)
	}
	filter.bCurrent = newBCurrent
	inputIndex = fmodOne(inputIndex)

	// Main loop
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincHexVariProcess: Starting main loop. Target output samples = %d\n", outCountSamples)
	}
	for outGenSamples < outCountSamples {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincHexVariProcess: Loop Iteration %d. outGenSamples=%d\n", outGenSamples/int64(state.channels), outGenSamples)
		}

		// Samples available
		if filter.bEnd >= filter.bCurrent {
			samplesInHand = filter.bEnd - filter.bCurrent
		} else {
			samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
		}
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincHexVariProcess: samplesInHand=%d. Needed=%d\n", samplesInHand, halfFilterChanLen)
		}

		// Need more?
		if samplesInHand <= halfFilterChanLen {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincHexVariProcess: samplesInHand <= halfFilterChanLen. Calling prepareData.\n")
			}
			data.InputFramesUsed = inUsedSamples / int64(state.channels)
			errCode := prepareData(filter, state.channels, data, halfFilterChanLen)
			if errCode != ErrNoError {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] sincHexVariProcess: prepareData returned error: %d\n", errCode)
				}
				state.errCode = errCode
				return errCode
			}
			inUsedSamples = data.InputFramesUsed * int64(state.channels)
			if filter.bEnd >= filter.bCurrent {
				samplesInHand = filter.bEnd - filter.bCurrent
			} else {
				samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
			}
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincHexVariProcess: After prepareData: samplesInHand=%d, inUsedSamples=%d (data.InputFramesUsed=%d)\n", samplesInHand, inUsedSamples, data.InputFramesUsed)
			}
			if samplesInHand <= halfFilterChanLen {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] sincHexVariProcess: samplesInHand *still* <= halfFilterChanLen (%d <= %d). Breaking loop.\n", samplesInHand, halfFilterChanLen)
				}
				break
			}
		}

		// Check EOF
		if filter.bRealEnd >= 0 {
			terminate := 1.0/srcRatio + 1e-20                                  // Use current loop's srcRatio
			checkPosition := float64(filter.bCurrent) + inputIndex + terminate // Approximate position needed for next sample

			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] ... EOF Check: bRealEnd=%d, checkPosition(curr+idx+1/ratio)=%.2f\n", filter.bRealEnd, checkPosition)
			}
			if checkPosition >= float64(filter.bRealEnd) {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] ... Breaking loop due to EOF check (C logic).\n")
				}
				break // Break loop if EOF reached
			}
		}
		// Vary ratio
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

		// Calc params
		floatIncrement := float64(filter.indexInc) * minFloat64(srcRatio, 1.0)
		increment = doubleToFP(floatIncrement)
		if increment == 0 {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincHexVariProcess: ERROR: Calculated increment is zero (srcRatio=%.15f, floatInc=%.15f).\n", srcRatio, floatIncrement)
			}
			state.errCode = ErrBadSrcRatio
			return state.errCode
		}
		startFilterIndex = doubleToFP(inputIndex * floatIncrement)
		scaleFactor := floatIncrement / float64(filter.indexInc)

		// Get output slice
		outPos := int(outGenSamples)
		if outPos+state.channels > len(data.DataOut) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincHexVariProcess: WARNING: Output buffer full (outPos=%d, channels=%d, len=%d). Breaking loop.\n", outPos, state.channels, len(data.DataOut))
			}
			break
		}
		outputSlice := data.DataOut[outPos : outPos+state.channels]

		// Calc output frame
		calcOutputHex(filter, state.channels, increment, startFilterIndex, scaleFactor, outputSlice)
		outGenSamples += int64(state.channels)

		// Update input index
		if srcRatio <= 1e-10 {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincHexVariProcess: ERROR: srcRatio is zero or very small (%.15f), cannot advance input index.\n", srcRatio)
			}
			state.errCode = ErrBadSrcRatio
			return state.errCode
		}
		inputIndex += 1.0 / srcRatio

		// Advance buffer pointer
		intInputAdvance = psfLrint(inputIndex - fmodOne(inputIndex))
		newBCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
		if newBCurrent < 0 {
			newBCurrent += filter.bLen
		}
		filter.bCurrent = newBCurrent
		inputIndex = fmodOne(inputIndex)

	} // End main loop

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincHexVariProcess: Exited main loop.\n")
	}

	// Store final state
	state.lastPosition = inputIndex
	state.lastRatio = srcRatio
	data.OutputFramesGen = outGenSamples / int64(state.channels)
	data.InputFramesUsed = inUsedSamples / int64(state.channels)

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincHexVariProcess: EXIT - data.OutGen=%d, data.InUsed=%d, state.lastPos=%.5f\n", data.OutputFramesGen, data.InputFramesUsed, state.lastPosition)
	}

	if state.errCode == ErrNoError {
		return ErrNoError
	}
	return state.errCode
}

// sincMultichanVariProcess handles generic multi-channel audio data.
// Corresponds to sinc_multichan_vari_process in src_sinc.c
func sincMultichanVariProcess(state *srcState, data *SrcData) ErrorCode {
	if sincDebugEnabled {
		fmt.Printf("\n[SINC_DEBUG] sincMultichanVariProcess: ENTRY - data.InFrames=%d, data.OutFrames=%d, data.SrcRatio=%.5f, data.EOF=%t\n",
			data.InputFrames, data.OutputFrames, data.SrcRatio, data.EndOfInput)
		fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: State - chans=%d, lastRatio=%.5f, lastPos=%.5f\n", state.channels, state.lastRatio, state.lastPosition)
	}

	filter, ok := state.privateData.(*sincFilter)
	if !ok || filter == nil {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: ERROR: Invalid private data.\n")
		}
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

	// Init ratio
	if isBadSrcRatio(srcRatio) {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: Initializing srcRatio from data.SrcRatio (%.5f)\n", data.SrcRatio)
		}
		if isBadSrcRatio(data.SrcRatio) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: ERROR: Bad initial srcRatio from data.\n")
			}
			return ErrBadSrcRatio
		}
		srcRatio = data.SrcRatio
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: Effective srcRatio for start = %.5f\n", srcRatio)
	}

	// Calc lookback/ahead
	filterCoeffsLen := float64(filter.coeffHalfLen + 2)
	if filter.indexInc <= 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: ERROR: Bad filter.indexInc (%d).\n", filter.indexInc)
		}
		return ErrBadInternalState
	}
	count := filterCoeffsLen / float64(filter.indexInc)
	effectiveMinRatio := srcRatio
	if !isBadSrcRatio(state.lastRatio) {
		effectiveMinRatio = minFloat64(state.lastRatio, srcRatio)
	}
	if effectiveMinRatio < (1.0 / srcMaxRatio) {
		effectiveMinRatio = 1.0 / srcMaxRatio
	}
	if effectiveMinRatio < 1.0 && effectiveMinRatio > 1e-10 {
		count /= effectiveMinRatio
	} else if effectiveMinRatio <= 1e-10 {
		count *= srcMaxRatio
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: WARNING: Very small minRatio (%.5f), using large lookback factor.\n", effectiveMinRatio)
		}
	}
	halfFilterChanLen = state.channels * (psfLrint(count) + 1)
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: Calculated halfFilterChanLen = %d\n", halfFilterChanLen)
	}

	// Advance buffer ptr
	intInputAdvance := psfLrint(inputIndex - fmodOne(inputIndex))
	if filter.bLen <= 0 {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: ERROR: Bad filter.bLen (%d).\n", filter.bLen)
		}
		return ErrBadInternalState
	}
	newBCurrent := (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
	if newBCurrent < 0 {
		newBCurrent += filter.bLen
	}
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: Advancing bCurrent by %d samples from %d to %d (modulo %d).\n", state.channels*intInputAdvance, filter.bCurrent, newBCurrent, filter.bLen)
	}
	filter.bCurrent = newBCurrent
	inputIndex = fmodOne(inputIndex)

	// Main loop
	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: Starting main loop. Target output samples = %d\n", outCountSamples)
	}
	for outGenSamples < outCountSamples {
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: Loop Iteration %d. outGenSamples=%d\n", outGenSamples/int64(state.channels), outGenSamples)
		}

		// Samples available
		if filter.bEnd >= filter.bCurrent {
			samplesInHand = filter.bEnd - filter.bCurrent
		} else {
			samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
		}
		if sincDebugEnabled {
			fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: samplesInHand=%d. Needed=%d\n", samplesInHand, halfFilterChanLen)
		}

		// Need more?
		if samplesInHand <= halfFilterChanLen {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: samplesInHand <= halfFilterChanLen. Calling prepareData.\n")
			}
			data.InputFramesUsed = inUsedSamples / int64(state.channels)
			errCode := prepareData(filter, state.channels, data, halfFilterChanLen)
			if errCode != ErrNoError {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: prepareData returned error: %d\n", errCode)
				}
				state.errCode = errCode
				return errCode
			}
			inUsedSamples = data.InputFramesUsed * int64(state.channels)
			if filter.bEnd >= filter.bCurrent {
				samplesInHand = filter.bEnd - filter.bCurrent
			} else {
				samplesInHand = (filter.bEnd + filter.bLen) - filter.bCurrent
			}
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: After prepareData: samplesInHand=%d, inUsedSamples=%d (data.InputFramesUsed=%d)\n", samplesInHand, inUsedSamples, data.InputFramesUsed)
			}
			if samplesInHand <= halfFilterChanLen {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: samplesInHand *still* <= halfFilterChanLen (%d <= %d). Breaking loop.\n", samplesInHand, halfFilterChanLen)
				}
				break
			}
		}

		// Check EOF
		if filter.bRealEnd >= 0 {
			terminate := 1.0/srcRatio + 1e-20                                  // Use current loop's srcRatio
			checkPosition := float64(filter.bCurrent) + inputIndex + terminate // Approximate position needed for next sample

			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] ... EOF Check: bRealEnd=%d, checkPosition(curr+idx+1/ratio)=%.2f\n", filter.bRealEnd, checkPosition)
			}
			if checkPosition >= float64(filter.bRealEnd) {
				if sincDebugEnabled {
					fmt.Printf("[SINC_DEBUG] ... Breaking loop due to EOF check (C logic).\n")
				}
				break // Break loop if EOF reached
			}
		}

		// Vary ratio
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

		// Calc params
		floatIncrement := float64(filter.indexInc) * minFloat64(srcRatio, 1.0)
		increment = doubleToFP(floatIncrement)
		if increment == 0 {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: ERROR: Calculated increment is zero (srcRatio=%.15f, floatInc=%.15f).\n", srcRatio, floatIncrement)
			}
			state.errCode = ErrBadSrcRatio
			return state.errCode
		}
		startFilterIndex = doubleToFP(inputIndex * floatIncrement)
		scaleFactor := floatIncrement / float64(filter.indexInc)

		// Get output slice
		outPos := int(outGenSamples)
		if outPos+state.channels > len(data.DataOut) {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: WARNING: Output buffer full (outPos=%d, channels=%d, len=%d). Breaking loop.\n", outPos, state.channels, len(data.DataOut))
			}
			break
		}
		outputSlice := data.DataOut[outPos : outPos+state.channels]

		calcOutputMulti(filter, state.channels, increment, startFilterIndex, scaleFactor, outputSlice)
		outGenSamples += int64(state.channels)

		// Update input index
		if srcRatio <= 1e-10 {
			if sincDebugEnabled {
				fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: ERROR: srcRatio is zero or very small (%.15f), cannot advance input index.\n", srcRatio)
			}
			state.errCode = ErrBadSrcRatio
			return state.errCode
		}
		inputIndex += 1.0 / srcRatio

		// Advance buffer pointer
		intInputAdvance = psfLrint(inputIndex - fmodOne(inputIndex))
		newBCurrent = (filter.bCurrent + state.channels*intInputAdvance) % filter.bLen
		if newBCurrent < 0 {
			newBCurrent += filter.bLen
		}
		filter.bCurrent = newBCurrent
		inputIndex = fmodOne(inputIndex)

	} // End main loop

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: Exited main loop.\n")
	}

	// Store final state
	state.lastPosition = inputIndex
	state.lastRatio = srcRatio
	data.OutputFramesGen = outGenSamples / int64(state.channels)
	data.InputFramesUsed = inUsedSamples / int64(state.channels) // Ensure this reflects total consumed

	if sincDebugEnabled {
		fmt.Printf("[SINC_DEBUG] sincMultichanVariProcess: EXIT - data.OutGen=%d, data.InUsed=%d, state.lastPos=%.5f\n", data.OutputFramesGen, data.InputFramesUsed, state.lastPosition)
	}

	if state.errCode == ErrNoError {
		return ErrNoError
	}
	return state.errCode
}

// --- End of Sinc Processing Functions ---
