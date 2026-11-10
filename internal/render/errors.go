package render

import "errors"

var (
	errEmptySpan        = errors.New("span produced no output frames")
	errNegativePosition = errors.New("crossfade produced a negative output position")
	errInvalidMix       = errors.New("mix dimensions are invalid")
	errMixTooLarge      = errors.New("mix is too large for this process")
)
