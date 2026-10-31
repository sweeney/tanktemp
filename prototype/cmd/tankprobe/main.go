// Command tankprobe is a throwaway prototype: it discovers DS18B20 sensors on
// every w1 bus, reads them with the onewire package's checks and retries, and
// prints one JSON line per sweep.
//
//	tankprobe -once
//	tankprobe -sites sites.conf -interval 30s -parallel
//
// sites.conf maps sensor IDs to positions on the cylinder, top first:
//
//	# id               name     height_cm
//	28-0000117287e6    top      150
//	28-00001171f570    upper    110
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sweeney/tanktemp/prototype/onewire"
)

type site struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	HeightCM int    `json:"height_cm,omitempty"`
}

type siteReading struct {
	site
	Bus      string   `json:"bus,omitempty"`
	TempC    *float64 `json:"temp_c"` // null when the read failed
	Attempts int      `json:"attempts"`
	TookMS   int64    `json:"took_ms"`
	Error    string   `json:"error,omitempty"`
}

type sweep struct {
	Timestamp  string        `json:"timestamp"`
	DurationMS int64         `json:"duration_ms"`
	Sites      []siteReading `json:"sites"`
	Unexpected []string      `json:"unexpected,omitempty"` // on the bus but not in sites.conf
}

func main() {
	root := flag.String("root", onewire.DevicesDir, "w1 devices directory")
	sitesPath := flag.String("sites", "", "sites file (default: every DS18B20 found, by ID)")
	interval := flag.Duration("interval", 30*time.Second, "time between sweeps")
	attempts := flag.Int("attempts", 3, "reads per sensor before giving up")
	parallel := flag.Bool("parallel", false, "read each bus in its own goroutine")
	once := flag.Bool("once", false, "do one sweep and exit")
	flag.Parse()

	if err := run(*root, *sitesPath, *interval, *attempts, *parallel, *once); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(root, sitesPath string, interval time.Duration, attempts int, parallel, once bool) error {
	var sites []site
	if sitesPath != "" {
		var err error
		if sites, err = loadSites(sitesPath); err != nil {
			return err
		}
	}
	sys := onewire.NewSysFS(root)
	enc := json.NewEncoder(os.Stdout)
	for {
		start := time.Now()
		sw, err := doSweep(sys, sites, attempts, parallel)
		if err != nil {
			log.Printf("sweep error: %v", err)
		} else if err := enc.Encode(sw); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
		if once {
			return nil
		}
		// Align to the interval so timestamps land on round numbers.
		time.Sleep(time.Until(start.Truncate(interval).Add(interval)))
	}
}

func doSweep(sys onewire.SysFS, sites []site, attempts int, parallel bool) (sweep, error) {
	start := time.Now()
	byBus, err := sys.Thermometers()
	if err != nil {
		return sweep{}, fmt.Errorf("discover: %w", err)
	}
	busOf := map[string]string{}
	for bus, ids := range byBus {
		for _, id := range ids {
			busOf[id] = bus
		}
	}

	known := map[string]bool{}
	if sites == nil {
		for id := range busOf {
			sites = append(sites, site{ID: id})
		}
		sort.Slice(sites, func(i, j int) bool { return sites[i].ID < sites[j].ID })
	}
	// Read only configured sensors, grouped by the bus they're on right now.
	// A configured sensor that has vanished is still read, so it reports
	// ErrNotPresent rather than silently disappearing from the output.
	want := map[string][]string{}
	for _, s := range sites {
		known[s.ID] = true
		want[busOf[s.ID]] = append(want[busOf[s.ID]], s.ID)
	}
	readings := sys.ReadAll(want, attempts, parallel)

	sw := sweep{Timestamp: start.UTC().Format(time.RFC3339)}
	for _, s := range sites {
		r := readings[s.ID]
		sr := siteReading{site: s, Bus: busOf[s.ID], Attempts: r.Attempts, TookMS: r.Took.Milliseconds()}
		if r.Err != nil {
			sr.Error = r.Err.Error()
		} else {
			c := r.Celsius()
			sr.TempC = &c
		}
		sw.Sites = append(sw.Sites, sr)
	}
	for id := range busOf {
		if !known[id] {
			sw.Unexpected = append(sw.Unexpected, id)
		}
	}
	sort.Strings(sw.Unexpected)
	sw.DurationMS = time.Since(start).Milliseconds()
	return sw, nil
}

func loadSites(path string) ([]site, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var sites []site
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if !strings.HasPrefix(fields[0], onewire.FamilyDS18B20+"-") || len(fields[0]) != 15 {
			return nil, fmt.Errorf("%s:%d: %q is not a DS18B20 id (28-xxxxxxxxxxxx)", path, n, fields[0])
		}
		if seen[fields[0]] {
			return nil, fmt.Errorf("%s:%d: duplicate id %s", path, n, fields[0])
		}
		seen[fields[0]] = true
		s := site{ID: fields[0]}
		if len(fields) > 1 {
			s.Name = fields[1]
		}
		if len(fields) > 2 {
			h, err := strconv.Atoi(fields[2])
			if err != nil {
				return nil, fmt.Errorf("%s:%d: height %q: %w", path, n, fields[2], err)
			}
			s.HeightCM = h
		}
		sites = append(sites, s)
	}
	return sites, sc.Err()
}
