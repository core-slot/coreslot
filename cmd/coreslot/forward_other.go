//go:build !darwin

package main

// hasForward says that this build has no forward subcommand: the forwarder exists on macOS alone.
const hasForward = false

func forwardCommand([]string) int { return exitUsage }
