package extension

import _ "embed"

// Source is this package's source code. tego builds Go extensions with
// it, so they need no download and use the protocol of the tego that runs
// them.
//
//go:embed extension.go
var Source string
