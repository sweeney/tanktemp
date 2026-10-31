// Command w1capture snapshots the 1-Wire sysfs files tanktemp reads into a
// fixture directory, so real hardware states become test cases. It is
// read-only on the Pi: reading w1_slave triggers a conversion, nothing more.
//
//	w1capture -out testdata/sysfs/steady
//	w1capture -out testdata/sysfs/unplug -steps 6 -interval 10s
//
// With -steps > 1 each snapshot goes in its own step-NNN directory, replayed
// one step per read by the onewire tests.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/sweeney/tanktemp/internal/onewire"
)

func main() {
	src := os.DirFS(onewire.DevicesDir)
	if root := os.Getenv("W1_ROOT"); root != "" {
		src = os.DirFS(root)
	}
	if err := run(os.Args[1:], src, time.Sleep); err != nil {
		log.Fatal(err)
	}
}

func run(args []string, src fs.FS, sleep func(time.Duration)) error {
	fl := flag.NewFlagSet("w1capture", flag.ContinueOnError)
	out := fl.String("out", "", "fixture directory to create (must be empty or absent)")
	steps := fl.Int("steps", 1, "number of snapshots")
	interval := fl.Duration("interval", 10*time.Second, "time between snapshots")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("-out is required")
	}
	for i := 1; i <= *steps; i++ {
		dst := *out
		if *steps > 1 {
			dst = filepath.Join(*out, fmt.Sprintf("step-%03d", i))
		}
		if err := onewire.Capture(src, dst); err != nil {
			return err
		}
		log.Printf("captured %s", dst)
		if i < *steps {
			sleep(*interval)
		}
	}
	return nil
}
