// Package onewire reads DS18B20 temperature sensors through the Linux w1
// subsystem's sysfs interface (/sys/bus/w1/devices) and decides whether each
// reading can be trusted.
//
// The kernel's CRC check is necessary but not sufficient: two failure modes
// produce scratchpads with a valid CRC (see ErrBusLow and ErrPowerOnReset).
package onewire

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	// ErrMalformed: the w1_slave text didn't have the expected two-line shape.
	ErrMalformed = errors.New("malformed w1_slave output")

	// ErrCRC: the scratchpad failed its CRC (kernel said NO, or our own check
	// disagreed). Usually noise or a marginal connection; retry.
	ErrCRC = errors.New("scratchpad CRC mismatch")

	// ErrBusLow: every byte read as 0x00. The data line was held low (pull-up
	// lost, DQ shorted to ground). All-zero bytes have CRC 0x00, so the kernel
	// reports "crc=00 YES" and t=0 — a fake 0 °C. Seen 2026-09-28 and 2026-10-18.
	ErrBusLow = errors.New("data line held low (all-zero scratchpad)")

	// ErrBadScratchpad: CRC passed but fixed-value bytes are wrong, so this
	// isn't a sane DS18B20 scratchpad.
	ErrBadScratchpad = errors.New("scratchpad fails DS18B20 sanity checks")

	// ErrPowerOnReset: the sensor restarted (lost power) and hasn't completed a
	// conversion yet, so it reports its power-up value of 85 °C. Seen 2026-09-28.
	ErrPowerOnReset = errors.New("power-on reset value (85 °C), not a measurement")

	// ErrOutOfRange: outside the DS18B20's -55..+125 °C range.
	ErrOutOfRange = errors.New("temperature outside DS18B20 range")
)

// Scratchpad is the DS18B20's 9-byte memory, as printed by the kernel.
//
//	0,1  temperature LSB, MSB (signed, 1/16 °C)
//	2,3  TH, TL alarm bytes (user EEPROM; ours are 0x4b 0x46 = factory 75/70)
//	4    configuration: 0b0RR11111, RR = resolution (11 → 12-bit)
//	5    reserved, always 0xFF
//	6    reserved; genuine parts: 0x0C at power-up, else 0x10 - (LSB & 0x0F)
//	7    reserved, always 0x10
//	8    CRC-8 of bytes 0..7
type Scratchpad [9]byte

// Raw returns the signed temperature register in 1/16 °C units.
func (s Scratchpad) Raw() int16 { return int16(uint16(s[1])<<8 | uint16(s[0])) }

// MilliC converts the temperature register exactly as the kernel's w1_therm
// does (integer millidegrees, truncated).
func (s Scratchpad) MilliC() int { return int(s.Raw()) * 1000 / 16 }

// Resolution in bits (9..12), from the configuration byte.
func (s Scratchpad) Resolution() int { return 9 + int(s[4]>>5&0x03) }

// CRCValid reports whether byte 8 is the Dallas/Maxim CRC-8 of bytes 0..7.
func (s Scratchpad) CRCValid() bool { return CRC8(s[:8]) == s[8] }

// Check classifies the scratchpad. A nil result means the temperature is a
// real measurement.
func (s Scratchpad) Check() error {
	if s == (Scratchpad{}) {
		return ErrBusLow
	}
	if !s.CRCValid() {
		return ErrCRC
	}
	// Bits 0-4 of the config byte read as 1 and bit 7 as 0 on every DS18B20;
	// bytes 5 and 7 are fixed.
	if s[4]&0x9F != 0x1F || s[5] != 0xFF || s[7] != 0x10 {
		return ErrBadScratchpad
	}
	// 0x0550 is 85 °C. At power-up byte 6 is 0x0C; after a real conversion that
	// reads 85.000 °C it would be 0x10 - 0 = 0x10. This lets a real 85 °C (which
	// a hot water cylinder can reach) through while rejecting the reset value.
	// The 0x10-(LSB&0x0F) pattern held in every raw scratchpad we inspected (all 10
	// sensors); the 0x0C power-up value is from the datasheet, not yet observed by us.
	// Clones may differ — see ONEWIRE.md.
	if s.Raw() == 0x0550 && s[6] == 0x0C {
		return ErrPowerOnReset
	}
	if mc := s.MilliC(); mc < -55000 || mc > 125000 {
		return ErrOutOfRange
	}
	return nil
}

// CRC8 is the Dallas/Maxim 1-Wire CRC (polynomial x^8+x^5+x^4+1, reflected 0x8C).
// It's used for both ROM IDs and scratchpads.
func CRC8(data []byte) byte {
	var crc byte
	for _, b := range data {
		for i := 0; i < 8; i++ {
			mix := (crc ^ b) & 0x01
			crc >>= 1
			if mix != 0 {
				crc ^= 0x8C
			}
			b >>= 1
		}
	}
	return crc
}

// W1Slave is a parsed /sys/bus/w1/devices/<id>/w1_slave file:
//
//	27 01 4b 46 7f ff 09 10 72 : crc=72 YES
//	27 01 4b 46 7f ff 09 10 72 t=18437
type W1Slave struct {
	Scratchpad Scratchpad
	KernelCRC  bool // the kernel's YES/NO verdict
	KernelT    int  // the kernel's t= value, millidegrees
}

// ParseW1Slave parses the two-line w1_slave text.
func ParseW1Slave(data []byte) (W1Slave, error) {
	var w W1Slave
	lines := strings.Split(strings.TrimSpace(string(bytes.TrimRight(data, "\x00"))), "\n")
	if len(lines) != 2 {
		return w, fmt.Errorf("%w: want 2 lines, got %d", ErrMalformed, len(lines))
	}

	left, verdict, ok := strings.Cut(lines[0], " : crc=")
	if !ok {
		return w, fmt.Errorf("%w: no crc field", ErrMalformed)
	}
	fields := strings.Fields(left)
	if len(fields) != 9 {
		return w, fmt.Errorf("%w: want 9 bytes, got %d", ErrMalformed, len(fields))
	}
	for i, f := range fields {
		b, err := strconv.ParseUint(f, 16, 8)
		if err != nil {
			return w, fmt.Errorf("%w: byte %d %q", ErrMalformed, i, f)
		}
		w.Scratchpad[i] = byte(b)
	}
	switch {
	case strings.HasSuffix(verdict, " YES"):
		w.KernelCRC = true
	case strings.HasSuffix(verdict, " NO"):
	default:
		return w, fmt.Errorf("%w: crc verdict %q", ErrMalformed, verdict)
	}

	_, t, ok := strings.Cut(lines[1], " t=")
	if !ok {
		return w, fmt.Errorf("%w: no t= field", ErrMalformed)
	}
	mc, err := strconv.Atoi(strings.TrimSpace(t))
	if err != nil {
		return w, fmt.Errorf("%w: t=%q", ErrMalformed, t)
	}
	w.KernelT = mc
	return w, nil
}

// Check combines the kernel's verdict with our own scratchpad checks.
func (w W1Slave) Check() error {
	if !w.KernelCRC {
		if w.Scratchpad == (Scratchpad{}) {
			return ErrBusLow
		}
		return ErrCRC
	}
	return w.Scratchpad.Check()
}
