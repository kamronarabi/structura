package scan

// SilencePanicStacks redirects the panic stack traces the pipeline writes to
// stderr into nowhere, for the duration of a test that deliberately panics.
// The traces are valuable in production — they are how a user reports an
// extractor bug — but in test output they bury the assertions.
func SilencePanicStacks() func() {
	previous := debugStack
	debugStack = func(string, string, any) {}
	return func() { debugStack = previous }
}
