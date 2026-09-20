//go:build !race

package resolve_test

// raceEnabled reports whether the race detector is active. Timing assertions
// are meaningless under it: instrumentation costs roughly an order of
// magnitude, so a threshold tight enough to catch a real regression would
// fail every race build.
const raceEnabled = false
