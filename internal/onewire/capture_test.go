package onewire

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestCaptureCopiesOnlyWhatTheHandlerReads(t *testing.T) {
	good := slave(0x10, 0x01, 0x4b, 0x46, 0x7f, 0xff, 0x10, 0x10)
	src := fstest.MapFS{
		"w1_bus_master1/w1_master_slaves":      {Data: []byte("28-00001171f570\n00-e00000000000\n")},
		"w1_bus_master1/w1_master_slave_count": {Data: []byte("2\n")},
		"w1_bus_master1/w1_master_search":      {Data: []byte("-1\n")},
		"w1_bus_master2/w1_master_slaves":      {Data: []byte("not found.\n")},
		"28-00001171f570/w1_slave":             {Data: []byte(good)},
		"28-00001171f570/ext_power":            {Data: []byte("1\n")},
		"28-00001171f570/resolution":           {Data: []byte("12\n")},
		"28-00001171f570/temperature":          {Data: []byte("17000\n")},
		"28-00001171f570/id":                   {Data: []byte{0x28, 0x70, 0xf5, 0x71, 0x11, 0, 0, 0x99}},
		"00-e00000000000/name":                 {Data: []byte("00-e00000000000\n")},
	}
	dst := t.TempDir()
	if err := Capture(src, dst); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(listFiles(t, dst), "\n")
	want := strings.Join([]string{
		"28-00001171f570/ext_power",
		"28-00001171f570/resolution",
		"28-00001171f570/w1_slave",
		"w1_bus_master1/w1_master_slaves",
		"w1_bus_master2/w1_master_slaves",
	}, "\n")
	if got != want {
		t.Errorf("captured:\n%s\nwant:\n%s", got, want)
	}
	b, _ := os.ReadFile(filepath.Join(dst, "28-00001171f570/w1_slave"))
	if string(b) != good {
		t.Errorf("w1_slave = %q, want byte-for-byte copy", b)
	}
	// The capture must read back through the handler like the original.
	th, err := SysFS{FS: os.DirFS(dst)}.Thermometers()
	if err != nil || len(th["w1_bus_master1"]) != 1 {
		t.Errorf("Thermometers over the capture = %v, %v", th, err)
	}
}

func TestCaptureSkipsMissingOptionalFiles(t *testing.T) {
	// A device that vanishes mid-capture, or an older kernel without
	// ext_power, must not abort the capture.
	src := fstest.MapFS{
		"w1_bus_master1/w1_master_slaves": {Data: []byte("28-000011721aa5\n28-0000deadbeef\n")},
		"28-000011721aa5/w1_slave":        {Data: []byte("00 00 00 00 00 00 00 00 00 : crc=00 YES\n00 00 00 00 00 00 00 00 00 t=0\n")},
	}
	dst := t.TempDir()
	if err := Capture(src, dst); err != nil {
		t.Fatal(err)
	}
	if got := listFiles(t, dst); len(got) != 2 {
		t.Errorf("captured %v", got)
	}
}

func TestCaptureRefusesANonEmptyDestination(t *testing.T) {
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Capture(fstest.MapFS{}, dst); err == nil {
		t.Error("want an error rather than mixing two captures")
	}
}
