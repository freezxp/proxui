package telemetry

import (
	"fmt"
	"sort"
	"strings"
)

// SensorKind is what a reading measures. `sensors` reports more than heat, and
// the kind is stored so a fan or a voltage can be recorded later without
// another migration — but only temperatures are collected today (ADR 0007).
type SensorKind string

const (
	// SensorTemp is a temperature in degrees Celsius.
	SensorTemp SensorKind = "temp_c"
	// SensorFan is a fan speed in RPM.
	SensorFan SensorKind = "fan_rpm"
)

// Reading is one sensor on one node at one moment.
//
// Chip and Label are kept as the hardware reports them — "coretemp-isa-0000"
// and "Package id 0" — rather than normalized into a vocabulary of the
// portal's own. A name invented here would have to be mapped back to the one
// the operator sees when they run `sensors` themselves, and the mapping would
// be wrong on the first board nobody tested.
type Reading struct {
	Chip  string     `json:"chip"`
	Label string     `json:"label"`
	Kind  SensorKind `json:"kind"`
	Value float64    `json:"value"`
	// High and Crit are the thresholds the chip declares for itself, absent on
	// hardware that declares none. They travel with the reading because 80°C
	// means something different on a CPU package than on a VRM, and only the
	// chip knows which this is.
	High *float64 `json:"high,omitempty"`
	Crit *float64 `json:"crit,omitempty"`
}

// Name is the reading in one string, for a chart legend or a message.
func (r Reading) Name() string { return r.Chip + " " + r.Label }

// Headroom is how far the reading is from the chip's critical point, as a
// fraction from 0 (at or past critical) to 1 (cold), and false when the chip
// declares no critical point.
//
// This is the portable comparison. A rule written against a raw temperature
// holds only for the machine it was written on; one written against headroom
// holds across an estate whose CPUs disagree about what hot means.
func (r Reading) Headroom() (float64, bool) {
	if r.Crit == nil || *r.Crit <= 0 {
		return 0, false
	}
	left := (*r.Crit - r.Value) / *r.Crit
	if left < 0 {
		left = 0
	}
	return left, true
}

// Hottest returns the reading closest to its own critical point, falling back
// to the highest raw temperature among readings whose chips declare none.
//
// "Closest to critical" rather than "highest" because the highest number on a
// board is routinely a sensor that is meant to be hot: a VRM at 75°C with a
// 125°C limit is idling, and it must not outrank a CPU package at 70°C with a
// 84°C limit, which is the one worth waking up for.
func Hottest(readings []Reading) (Reading, bool) {
	var best Reading
	var bestScore float64
	found := false
	rated := false

	for _, r := range readings {
		if r.Kind != SensorTemp {
			continue
		}
		headroom, ok := r.Headroom()
		switch {
		case ok && !rated:
			// The first reading with a critical point displaces any number of
			// unrated ones: a rated comparison is the better one.
			best, bestScore, rated, found = r, headroom, true, true
		case ok && headroom < bestScore:
			best, bestScore = r, headroom
		case !ok && !rated && (!found || r.Value > bestScore):
			best, bestScore, found = r, r.Value, true
		}
	}
	return best, found
}

// SortReadings orders readings for display: by chip, then by label, with
// numeric suffixes in numeric order so "Core 10" follows "Core 9".
func SortReadings(readings []Reading) {
	sort.SliceStable(readings, func(i, j int) bool {
		if readings[i].Chip != readings[j].Chip {
			return readings[i].Chip < readings[j].Chip
		}
		return lessLabel(readings[i].Label, readings[j].Label)
	})
}

// lessLabel compares labels that usually differ only by a trailing number.
func lessLabel(a, b string) bool {
	aStem, aNum, aOK := splitTrailingNumber(a)
	bStem, bNum, bOK := splitTrailingNumber(b)
	if aOK && bOK && aStem == bStem {
		return aNum < bNum
	}
	return a < b
}

// splitTrailingNumber splits "Core 10" into "Core " and 10.
func splitTrailingNumber(s string) (string, int, bool) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) || i == 0 {
		return s, 0, false
	}
	n := 0
	if _, err := fmt.Sscanf(s[i:], "%d", &n); err != nil {
		return s, 0, false
	}
	return s[:i], n, true
}

// SensorSummary is what a host page shows without asking for every reading.
type SensorSummary struct {
	// Hottest is the reading nearest its own limit, empty when nothing was read.
	Hottest Reading `json:"hottest"`
	Count   int     `json:"count"`
	// Chips names the hardware that answered, for the "3 chips, 14 sensors"
	// line that tells an operator the collection is working.
	Chips []string `json:"chips"`
}

// Summarize reduces a node's readings to what a list row can hold.
func Summarize(readings []Reading) SensorSummary {
	out := SensorSummary{Count: len(readings)}
	if hottest, ok := Hottest(readings); ok {
		out.Hottest = hottest
	}
	seen := map[string]bool{}
	for _, r := range readings {
		if r.Chip != "" && !seen[r.Chip] {
			seen[r.Chip] = true
			out.Chips = append(out.Chips, r.Chip)
		}
	}
	sort.Strings(out.Chips)
	return out
}

// ShortChip trims the bus suffix `sensors` appends, so "coretemp-isa-0000"
// reads as "coretemp" in a column that has no room for the rest.
func ShortChip(chip string) string {
	for _, sep := range []string{"-isa-", "-pci-", "-virtual-", "-acpi-", "-i2c-"} {
		if i := strings.Index(chip, sep); i > 0 {
			return chip[:i]
		}
	}
	return chip
}
