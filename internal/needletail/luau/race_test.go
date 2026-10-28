//go:build race

package luau

// raceEnabled: the race detector instruments every interpreted memory access, so the tests that
// move megabytes through the module are skipped under it (they run in the normal suite).
const raceEnabled = true
