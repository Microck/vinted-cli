package cli

import (
	"context"
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

// UseServiceExec runs service manager commands through fn instead of the OS.
func UseServiceExec(fn func(name string, args ...string) error) (restore func()) {
	prev := serviceExec
	serviceExec = func(_ context.Context, name string, args ...string) error { return fn(name, args...) }
	return func() { serviceExec = prev }
}

// UsePromptTerminal makes the login's renewal prompt see a terminal (or not).
func UsePromptTerminal(tty bool) (restore func()) {
	prev := promptTerminal
	promptTerminal = func(*App) bool { return tty }
	return func() { promptTerminal = prev }
}
