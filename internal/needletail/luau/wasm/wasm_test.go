package wasm

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// SHA256 is luau.wasm's sum as build.sh printed it. Changing the module (shim.cpp, a Luau or
// wasi-sdk bump, a flag) means rebuilding it with build.sh and updating this pin in one commit.
const SHA256 = "5cb9fb4e1f8d134149aa5fdf71441e07168c74f945764e9015a820be21b61e97"

func TestModuleSHA256(t *testing.T) {
	sum := sha256.Sum256(Module)
	if got := hex.EncodeToString(sum[:]); got != SHA256 {
		t.Fatalf("luau.wasm sha256 = %s, pinned %s: rebuild with build.sh and update the pin together", got, SHA256)
	}
}
