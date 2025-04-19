// config.go
package libsamplerate

// Constants derived from config.h defines.

const (
	// CPU characteristics (may not be directly used unless needed for specific optimizations)
	cpuClipsNegative  = false // CPU_CLIPS_NEGATIVE 0
	cpuClipsPositive  = false // CPU_CLIPS_POSITIVE 0
	cpuIsBigEndian    = false // CPU_IS_BIG_ENDIAN 0
	cpuIsLittleEndian = true  // CPU_IS_LITTLE_ENDIAN 1

	// Enabled converters (based on 'yes' defines)
	enableSincBestConverter   = true // ENABLE_SINC_BEST_CONVERTER yes
	enableSincFastConverter   = true // ENABLE_SINC_FAST_CONVERTER yes
	enableSincMediumConverter = true // ENABLE_SINC_MEDIUM_CONVERTER yes

	// Feature availability (less relevant for pure Go, useful if using CGo)
	haveAlarm  = true
	haveAlsa   = true
	haveCalloc = true // Go uses GC
	haveCeil   = true // Use math.Ceil
	haveDlfcnH = true
	// haveFftw3 = false // Undefined
	haveFloor      = true // Use math.Floor
	haveFmod       = true // Use math.Mod
	haveFree       = true // Go uses GC
	haveImmIntrinH = true // Relevant for C SSE intrinsics
	haveIntTypesH  = true
	haveLrint      = true // Use custom PsfLrint or math.Round
	haveLrintf     = true // Use custom PsfLrintf or math.Round
	haveMalloc     = true // Go uses GC
	haveMemcpy     = true // Use Go copy()
	haveMemmove    = true // Use Go copy() (handles overlap)
	haveSigalrm    = true
	haveSignal     = true
	// haveSndfile = false // Undefined
	haveStdboolH   = true // Go has bool type
	haveStdintH    = true // Go has int types like int32, int64
	haveStdioH     = true // Use Go "fmt", "os", etc.
	haveStdlibH    = true // Use Go standard library
	haveStringsH   = true // Use Go "strings"
	haveStringH    = true // Use Go "strings", slices
	haveSysStatH   = true
	haveSysTimesH  = true
	haveSysTypesH  = true
	haveUnistdH    = true
	haveVisibility = true // Less relevant for Go visibility (uses exported/unexported)

	// Package information
	packageName      = "libsamplerate"
	packageBugReport = "erikd@mega-nerd.com"
	packageAppName   = "libsamplerate" // mapped from PACKAGE_NAME
	packageString    = "libsamplerate 0.2.2"
	packageTarname   = "libsamplerate"
	packageURL       = "https://github.com/libsndfile/libsamplerate/"
	packageVersion   = "0.2.2"

	// Basic type sizes (Go types have defined sizes, e.g., float64 is 8 bytes)
	// These might be useful if C interop was needed, but less so for pure Go.
	sizeofDouble = 8
	sizeofFloat  = 4
	sizeofInt    = 4 // Platform dependent in C, use specific Go types like int32
	sizeofLong   = 8 // Platform dependent in C, use int64

	stdcHeaders   = true
	VersionString = "0.2.2"
	// wordsBigEndian = false // Undefined unless under specific conditions
)

// Custom Go constants mirroring C definitions where appropriate
const (
	maxChannels = 128 // Hardcoded in src_sinc.c, seems reasonable default if not config specific
)

// Go bool equivalents
const (
	srcTrue  = true
	srcFalse = false
)
