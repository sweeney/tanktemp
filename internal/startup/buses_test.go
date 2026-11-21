package startup

import (
	"errors"
	"strings"
	"testing"
)

// CheckBuses compares the declared 1-Wire pins with the loaded ones (#10).
func TestCheckBuses(t *testing.T) {
	for _, tt := range []struct {
		name            string
		declared, found []int
		err             error
		want            []string // codes, in order
		mention         string
	}{
		{"nothing declared: no check", nil, []int{4}, nil, nil, ""},
		{"as declared", []int{4, 17}, []int{17, 4}, nil, nil, ""},
		{"one missing", []int{4, 17}, []int{4}, nil, []string{CodeW1BusMissing}, "GPIO 17"},
		{"one undeclared", []int{4}, []int{4, 22}, nil, []string{CodeW1BusUndeclared}, "GPIO 22"},
		{"both", []int{4}, []int{22}, nil, []string{CodeW1BusMissing, CodeW1BusUndeclared}, "GPIO 4"},
		{"unreadable", []int{4}, nil, errors.New("no device tree"), []string{CodeW1BusCheckFailed}, "no device tree"},
		{"unreadable, nothing declared", nil, nil, errors.New("no device tree"), nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckBuses(tt.declared, tt.found, tt.err)
			var codes []string
			all := ""
			for _, p := range got {
				codes = append(codes, p.Code)
				all += p.Message
				if p.ROM != "" {
					t.Errorf("a bus problem has a ROM: %+v", p)
				}
			}
			if strings.Join(codes, ",") != strings.Join(tt.want, ",") || !strings.Contains(all, tt.mention) {
				t.Errorf("CheckBuses = %+v; want codes %v mentioning %q", got, tt.want, tt.mention)
			}
		})
	}
}
