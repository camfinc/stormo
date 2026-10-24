// Package wasm holds luau.wasm, Luau's VM and compiler with needletail's shim (shim.cpp) built for
// wasm32-wasip1 by build.sh (docs/needletail.md §5.1). The module is embedded so the binary needs
// nothing at run time; its sha256 is pinned in a test, and build.sh rebuilds it reproducibly.
package wasm

import _ "embed"

// Module is luau.wasm.
//
//go:embed luau.wasm
var Module []byte
