//go:build hardware && linux

package gpioedge

// The hardware check for the meter's line: run on the Pi, never in CI. It
// requests the real line through the real Source and runs the shared
// contract (without a driver: nothing can work the reed but water), then
// checks a held line can't be requested twice. With TANKTEMP_METER_WATCH
// it also prints every edge for that long, while you run a tap.
//
//	GOOS=linux GOARCH=arm GOARM=6 go test -c -tags hardware ./internal/gpioedge
//	TANKTEMP_METER_LINE=gpiochip0:27 ./gpioedge.test -test.v
//	TANKTEMP_METER_LINE=gpiochip0:27 TANKTEMP_METER_WATCH=60s ./gpioedge.test -test.v -test.run Watch
//
// The line must be free: stop anything else holding it (gpiomon) first.

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/tanktemp/internal/meter"
	"github.com/sweeney/tanktemp/internal/meter/metertest"
)

func hwLine(t *testing.T) (string, int) {
	t.Helper()
	chip, off, ok := strings.Cut(os.Getenv("TANKTEMP_METER_LINE"), ":")
	n, err := strconv.Atoi(off)
	if !ok || err != nil {
		t.Fatal("set TANKTEMP_METER_LINE to chip:offset, e.g. gpiochip0:27")
	}
	return chip, n
}

func hwOpen(t *testing.T) meter.Source {
	t.Helper()
	chip, n := hwLine(t)
	s, err := Open(Settings{Chip: chip, Offset: n, Debounce: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestHardwareContract(t *testing.T) {
	metertest.Contract(t, metertest.Harness{Open: hwOpen})
}

func TestHardwareBusy(t *testing.T) {
	s := hwOpen(t)
	defer s.Close()
	chip, n := hwLine(t)
	if s2, err := Open(Settings{Chip: chip, Offset: n, Debounce: 20 * time.Millisecond}); err == nil {
		s2.Close()
		t.Fatal("a held line was requested again")
	} else if !strings.Contains(err.Error(), chip) {
		t.Errorf("the error doesn't name the chip: %v", err)
	}
}

func TestHardwareWatch(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv("TANKTEMP_METER_WATCH"))
	if err != nil {
		t.Skip("set TANKTEMP_METER_WATCH (e.g. 60s) to watch the line")
	}
	s := hwOpen(t)
	defer s.Close()
	t.Logf("initial level: %v; watching for %v", s.Initial(), d)
	end := time.After(d)
	for {
		select {
		case e := <-s.Edges():
			t.Logf("seqno %d at %v: %v", e.Seqno, e.At, e.Level)
		case <-end:
			return
		}
	}
}
