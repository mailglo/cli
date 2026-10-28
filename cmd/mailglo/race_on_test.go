//go:build race

package main

const raceEnabled = true

// size scales a stress test down under the race detector, which runs the
// CLI about ten times slower; timing checks are skipped there.
func size(n int) int { return n / 10 }
