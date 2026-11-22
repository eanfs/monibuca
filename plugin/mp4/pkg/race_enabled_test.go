//go:build race

package mp4

// raceEnabled reports whether tests are compiled with the race detector. It is
// used to skip cases that trigger the known gotask v1.0.5 EventLoop data race
// (CLAUDE.md Known Issues), matching plugin/mp4/race_enabled_test.go.
const raceEnabled = true
