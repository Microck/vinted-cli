// Package bundle identifies the exact upstream engine used by both the SDK and daemon.
package bundle

import _ "embed"

//go:embed LICENSE
var License []byte

const Module = "github.com/Microck/galleton"
const Version = "v0.1.0"
const Sum = "h1:pcgroA8gfunV0GNRt9n+Jrt3ra59IXgBF9pRI/VTpRU="
const ModSum = "h1:hp07LS66wDIysy70Z7P0Q7WOZEFP63hYsVC7gx4B+yk="

// EngineVersion is what the pinned daemon reports from /v1/health.
const EngineVersion = "0.1.0"
