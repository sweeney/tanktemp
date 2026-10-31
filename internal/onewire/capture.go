package onewire

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Captured files. Exactly what the handler reads, so a capture replays
// through SysFS like the live directory.
const (
	fileMasterSlaves = "w1_master_slaves"
	fileW1Slave      = "w1_slave"
	fileExtPower     = "ext_power"
	fileResolution   = "resolution"
)

// Capture copies the files the handler reads from src (laid out like
// /sys/bus/w1/devices) into dst, which must be empty or absent. It is
// read-only on src: reading w1_slave triggers a conversion, nothing more.
// Files missing on a device (it vanished mid-capture, or the kernel lacks
// them) are skipped rather than failing the capture.
func Capture(src fs.FS, dst string) error {
	if entries, err := os.ReadDir(dst); err == nil && len(entries) > 0 {
		return fmt.Errorf("capture: %s is not empty", dst)
	}
	masters, err := fs.Glob(src, "w1_bus_master*")
	if err != nil {
		return err
	}
	sort.Strings(masters)
	copyFile := func(name string, optional bool) error {
		data, err := fs.ReadFile(src, name)
		if err != nil {
			if optional && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("capture %s: %w", name, err)
		}
		p := filepath.Join(dst, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, data, 0o644)
	}
	var devices []string
	for _, m := range masters {
		if err := copyFile(m+"/"+fileMasterSlaves, false); err != nil {
			return err
		}
		data, _ := fs.ReadFile(src, m+"/"+fileMasterSlaves)
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, FamilyDS18B20+"-") {
				devices = append(devices, line)
			}
		}
	}
	sort.Strings(devices)
	for _, id := range devices {
		for _, f := range []string{fileW1Slave, fileExtPower, fileResolution} {
			if err := copyFile(id+"/"+f, true); err != nil {
				return err
			}
		}
	}
	return os.MkdirAll(dst, 0o755)
}
