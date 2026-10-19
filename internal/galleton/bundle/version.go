// Package bundle identifies the exact upstream engine used by both the SDK and daemon.
package bundle

import _ "embed"

//go:embed LICENSE
var License []byte

const Module = "github.com/Microck/galleton"
const Version = "v0.1.1"
const Sum = "h1:VL9/vGAwHZ/uZeE6tzx9M7Sgee0iMPr4KnCnv7jCGz8="
const ModSum = "h1:hp07LS66wDIysy70Z7P0Q7WOZEFP63hYsVC7gx4B+yk="

// EngineVersion is what the pinned daemon reports from /v1/health.
const EngineVersion = "0.1.1"
