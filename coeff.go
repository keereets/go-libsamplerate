//
// Copyright (c) 2025, Antonio Chirizzi <antonio.chirizzi@gmail.com>
// All rights reserved.
//
// This code is released under 3-clause BSD license. Please see the
// file LICENSE
//

package libsamplerate

// coeffData holds the coefficients and increment for a specific quality level.
type coeffData struct {
	Increment int
	Coeffs    []float32
}
