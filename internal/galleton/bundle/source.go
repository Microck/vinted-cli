//go:build !galleton_bundle

package bundle

// Source/go-install builds compile the pinned companion on first authenticated
// use. Release builds and make install embed it and need no runtime Go toolchain.
var Binary []byte
