package cli

import "github.com/Microck/vinted-cli/internal/throttle"

// UseThrottleClock runs the request gate on c until the returned func
// restores the real clock, so tests check pacing without sleeping.
func UseThrottleClock(c throttle.Clock) (restore func()) {
	prev := throttleClock
	throttleClock = c
	return func() { throttleClock = prev }
}
