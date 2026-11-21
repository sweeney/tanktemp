package onewire

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// deviceTree is where the kernel shows the live device tree, under a
// filesystem rooted at "/" (/proc/device-tree is a link to it).
const deviceTree = "sys/firmware/devicetree/base"

// BusPins returns the GPIO of each loaded w1-gpio overlay (#10), sorted:
// what config.txt's dtoverlay=w1-gpio,gpiopin=N lines made of the device
// tree at boot. Each overlay is a node "onewire@<pin in hex>" whose gpios
// property is <phandle pin flags>, big-endian 32-bit cells; the pin is read
// from the property, which is what the driver uses, not from the name.
//
// It says nothing about which w1_bus_masterN is which: there is no link
// from a master back to its node, and their numbering isn't stable
// (docs/ONEWIRE.md).
func BusPins(root fs.FS) ([]int, error) {
	entries, err := fs.ReadDir(root, deviceTree)
	if err != nil {
		return nil, fmt.Errorf("reading the device tree: %w", err)
	}
	var pins []int
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "onewire@") {
			continue
		}
		b, err := fs.ReadFile(root, deviceTree+"/"+e.Name()+"/gpios")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if len(b) < 8 {
			return nil, fmt.Errorf("%s: gpios is %d bytes, want at least 8 (phandle, pin)", e.Name(), len(b))
		}
		pins = append(pins, int(binary.BigEndian.Uint32(b[4:8])))
	}
	sort.Ints(pins)
	return pins, nil
}
