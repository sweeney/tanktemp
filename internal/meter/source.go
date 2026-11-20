package meter

// Source delivers one GPIO line's edges (internal/gpioedge on the Pi,
// metertest.Fake in tests). metertest.Contract is what each must meet.
type Source interface {
	// Initial is the line's level, read when it was requested.
	Initial() Level
	// Edges delivers each edge in the order the kernel numbered them,
	// stamped by the kernel on the monotonic clock. It is closed by Close.
	Edges() <-chan Edge
	// Close releases the line and closes Edges. A second Close is a no-op.
	Close() error
}
