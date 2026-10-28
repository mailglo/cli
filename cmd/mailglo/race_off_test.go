//go:build !race

package main

const raceEnabled = false

func size(n int) int { return n }
