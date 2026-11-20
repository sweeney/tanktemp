//go:build linux

package gpioedge

import (
	"sync"
	"time"

	"github.com/warthog618/go-gpiocdev"

	"github.com/sweeney/tanktemp/internal/meter"
)

// buffer is the edge channel's capacity.
const buffer = 256

type line struct {
	l       *gpiocdev.Line
	initial meter.Level
	edges   chan meter.Edge

	mu     sync.Mutex
	closed bool
}

func request(chip string, offset int, debounce time.Duration) (*line, error) {
	s := &line{edges: make(chan meter.Edge, buffer)}
	l, err := gpiocdev.RequestLine(chip, offset,
		gpiocdev.WithConsumer("tanktemp-meter"),
		gpiocdev.AsInput,
		gpiocdev.WithPullUp,
		gpiocdev.WithBothEdges,
		gpiocdev.WithDebounce(debounce),
		gpiocdev.WithMonotonicEventClock,
		gpiocdev.WithEventHandler(s.handle))
	if err != nil {
		return nil, err
	}
	v, err := l.Value()
	if err != nil {
		l.Close()
		return nil, err
	}
	s.l, s.initial = l, levelOf(v)
	return s, nil
}

// handle runs on go-gpiocdev's watcher goroutine. It never blocks: a full
// channel drops the edge, which the counter sees as a seqno gap (lost).
func (s *line) handle(e gpiocdev.LineEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.edges <- edgeOf(e.Timestamp, e.Type == gpiocdev.LineEventRisingEdge, e.LineSeqno):
	default:
	}
}

func (s *line) Initial() meter.Level     { return s.initial }
func (s *line) Edges() <-chan meter.Edge { return s.edges }

func (s *line) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.edges)
	s.mu.Unlock()
	return s.l.Close()
}
