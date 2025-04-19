// coeffs.go
package libsamplerate

// coeffData holds the coefficients and increment for a specific quality level.
type coeffData struct {
	Increment int
	Coeffs    []float32
}
