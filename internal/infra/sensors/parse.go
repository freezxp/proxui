// Package sensors turns `sensors -j` output into readings.
//
// The shape is three levels deep and not self-describing: chip, then feature,
// then subfeature, where the subfeature's *name* carries the type and role.
//
//	{"coretemp-isa-0000": {
//	    "Adapter": "ISA adapter",
//	    "Package id 0": {"temp1_input": 47.0, "temp1_max": 84.0, "temp1_crit": 100.0}}}
//
// So `temp1_input` is the reading, `temp1_max` and `temp1_crit` are its
// thresholds, and the digit ties them together — a feature can hold more than
// one numbered set. Everything here is that unpacking.
package sensors

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/freezxp/proxui/internal/domain/telemetry"
)

// Command is what the collector runs on a node. A constant, never assembled
// from anything a request supplied (ADR 0007).
const Command = "sensors -j"

// Parse reads `sensors -j` output.
//
// A chip that cannot be understood is skipped rather than failing the parse:
// one odd driver on one board must not cost every other reading on it. The
// error is reserved for output that is not the expected document at all,
// which is how a missing `lm-sensors` shows up — the shell answers with a
// message on stderr and nothing on stdout.
func Parse(out []byte) ([]telemetry.Reading, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, fmt.Errorf("sensors: no output; is lm-sensors installed on the node")
	}

	// The value is `any` rather than a typed map because a chip's entries are
	// a mix of nested objects and the plain "Adapter" string.
	var doc map[string]map[string]any
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		return nil, fmt.Errorf("sensors: output was not the expected JSON: %w", err)
	}

	readings := make([]telemetry.Reading, 0, 16)
	for chip, features := range doc {
		for label, raw := range features {
			// "Adapter": "ISA adapter" and anything else flat.
			values, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			readings = append(readings, feature(chip, label, values)...)
		}
	}
	telemetry.SortReadings(readings)
	return readings, nil
}

// feature turns one feature's subfeatures into readings, one per numbered set.
func feature(chip, label string, values map[string]any) []telemetry.Reading {
	// Group by the number in the subfeature name, so temp1_* and temp2_* under
	// one label do not blend into each other.
	type group struct {
		kind              telemetry.SensorKind
		input             *float64
		high, crit, fault *float64
	}
	groups := map[string]*group{}

	for name, raw := range values {
		kind, key, role, ok := subfeature(name)
		if !ok {
			continue
		}
		number, err := asFloat(raw)
		if err != nil {
			continue
		}
		g := groups[key]
		if g == nil {
			g = &group{kind: kind}
			groups[key] = g
		}
		switch role {
		case "input":
			g.input = &number
		case "max":
			// Only the first max wins: some drivers publish both `max` and
			// `emergency`, and the lower of the two is the useful one.
			if g.high == nil {
				g.high = &number
			}
		case "crit":
			g.crit = &number
		case "fault", "alarm":
			g.fault = &number
		}
	}

	out := make([]telemetry.Reading, 0, len(groups))
	for _, g := range groups {
		if g.input == nil {
			// A threshold with no reading beside it describes nothing.
			continue
		}
		// A chip reporting a fault is reporting that its own number is
		// untrustworthy, which is worse than having no number.
		if g.fault != nil && *g.fault != 0 {
			continue
		}
		// Disconnected probes read as exactly zero on several drivers, and a
		// 0°C CPU would be the coldest thing in the estate.
		if g.kind == telemetry.SensorTemp && *g.input == 0 {
			continue
		}
		out = append(out, telemetry.Reading{
			Chip: chip, Label: label, Kind: g.kind,
			Value: *g.input, High: g.high, Crit: g.crit,
		})
	}
	return out
}

// subfeature splits "temp1_crit" into its kind, its group key and its role.
func subfeature(name string) (telemetry.SensorKind, string, string, bool) {
	stem, role, found := strings.Cut(name, "_")
	if !found {
		return "", "", "", false
	}
	switch {
	case strings.HasPrefix(stem, "temp"):
		return telemetry.SensorTemp, stem, role, true
	case strings.HasPrefix(stem, "fan"):
		return telemetry.SensorFan, stem, role, true
	}
	return "", "", "", false
}

// asFloat accepts the number forms `sensors` emits. It writes plain JSON
// numbers, but a value it could not read comes through as null and some builds
// quote it.
func asFloat(raw any) (float64, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case json.Number:
		return v.Float64()
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err != nil {
			return 0, err
		}
		return f, nil
	}
	return 0, fmt.Errorf("sensors: %T is not a number", raw)
}
