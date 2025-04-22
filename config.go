//
// Copyright (c) 2025, Antonio Chirizzi <antonio.chirizzi@gmail.com>
// All rights reserved.
//
// This code is released under 3-clause BSD license. Please see the
// file LICENSE
//

package libsamplerate

// Constants derived from config.h defines.

const (
	// Enabled converters (based on 'yes' defines)
	enableSincBestConverter   = true // ENABLE_SINC_BEST_CONVERTER yes
	enableSincFastConverter   = true // ENABLE_SINC_FAST_CONVERTER yes
	enableSincMediumConverter = true // ENABLE_SINC_MEDIUM_CONVERTER yes

	packageVersion = "0.2.2"

	maxChannels = 128 // Hardcoded in src_sinc.c, seems reasonable default if not config specific
)
