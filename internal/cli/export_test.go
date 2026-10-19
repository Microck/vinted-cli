package cli

import (
	"time"

	"github.com/Microck/vinted-cli/internal/throttle"
)

// UseThrottleClock runs the request gate on c until the returned func
// restores the real clock, so tests check pacing without sleeping.
func UseThrottleClock(c throttle.Clock) (restore func()) {
	prev := throttleClock
	throttleClock = c
	return func() { throttleClock = prev }
}

// UseServiceLoop runs `auth service run` checks every tick until stop closes.
func UseServiceLoop(tick time.Duration, stop <-chan struct{}) (restore func()) {
	prevTick, prevStop := serviceTick, serviceStop
	serviceTick, serviceStop = tick, stop
	return func() { serviceTick, serviceStop = prevTick, prevStop }
}

// UseChatPoll lowers the `chat open --interval` floor until restored.
func UseChatPoll(floor time.Duration) (restore func()) {
	prev := minChatPoll
	minChatPoll = floor
	return func() { minChatPoll = prev }
}
