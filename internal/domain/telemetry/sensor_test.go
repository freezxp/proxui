package telemetry

import "testing"

func ptr(f float64) *float64 { return &f }

// The highest number on a board is routinely a sensor meant to be hot. What
// matters is which reading is nearest its own limit.
func TestHottestRanksByHeadroomNotByDegrees(t *testing.T) {
	vrm := Reading{Chip: "nct6798", Label: "VRM", Kind: SensorTemp, Value: 75, Crit: ptr(125)}
	cpu := Reading{Chip: "coretemp", Label: "Package id 0", Kind: SensorTemp, Value: 70, Crit: ptr(84)}

	got, ok := Hottest([]Reading{vrm, cpu})
	if !ok {
		t.Fatal("nothing was picked")
	}
	if got.Label != "Package id 0" {
		t.Errorf("picked %s at %g°C; the CPU has less headroom at %g°C",
			got.Label, got.Value, cpu.Value)
	}
}

// Hardware that declares no critical point still has to be rankable, or a
// board whose drivers publish no thresholds would report nothing at all.
func TestHottestFallsBackToDegreesWithoutThresholds(t *testing.T) {
	got, ok := Hottest([]Reading{
		{Chip: "acpitz", Label: "temp1", Kind: SensorTemp, Value: 27.8},
		{Chip: "acpitz", Label: "temp2", Kind: SensorTemp, Value: 41.2},
	})
	if !ok || got.Value != 41.2 {
		t.Errorf("got %+v, want the 41.2°C reading", got)
	}
}

// A rated reading is the better comparison, so it wins even when an unrated
// one shows a bigger number.
func TestARatedReadingOutranksAnUnratedOne(t *testing.T) {
	got, ok := Hottest([]Reading{
		{Chip: "acpitz", Label: "temp1", Kind: SensorTemp, Value: 90},
		{Chip: "coretemp", Label: "Package id 0", Kind: SensorTemp, Value: 50, Crit: ptr(100)},
	})
	if !ok || got.Chip != "coretemp" {
		t.Errorf("got %+v, want the reading that knows its own limit", got)
	}
}

func TestHottestIgnoresFans(t *testing.T) {
	got, ok := Hottest([]Reading{
		{Chip: "nct6798", Label: "fan1", Kind: SensorFan, Value: 1200},
		{Chip: "coretemp", Label: "Package id 0", Kind: SensorTemp, Value: 50},
	})
	if !ok || got.Kind != SensorTemp {
		t.Errorf("got %+v, want a temperature", got)
	}
	if _, ok := Hottest(nil); ok {
		t.Error("something was picked out of nothing")
	}
}

func TestHeadroom(t *testing.T) {
	r := Reading{Value: 50, Crit: ptr(100)}
	if got, ok := r.Headroom(); !ok || got != 0.5 {
		t.Errorf("headroom = %v (%v), want 0.5", got, ok)
	}
	// Past critical is zero headroom, not negative: nothing is worse than
	// past the limit, and a negative would sort below a working sensor.
	past := Reading{Value: 120, Crit: ptr(100)}
	if got, _ := past.Headroom(); got != 0 {
		t.Errorf("headroom past critical = %v, want 0", got)
	}
	if _, ok := (Reading{Value: 50}).Headroom(); ok {
		t.Error("a reading with no critical point reported headroom")
	}
}

// "Core 10" belongs after "Core 9", which string order gets wrong.
func TestSortIsNumericWithinALabel(t *testing.T) {
	readings := []Reading{
		{Chip: "coretemp", Label: "Core 10"},
		{Chip: "coretemp", Label: "Core 9"},
		{Chip: "coretemp", Label: "Core 1"},
		{Chip: "acpitz", Label: "temp1"},
	}
	SortReadings(readings)
	want := []string{"temp1", "Core 1", "Core 9", "Core 10"}
	for i, w := range want {
		if readings[i].Label != w {
			t.Fatalf("order = %v, want %v", labels(readings), want)
		}
	}
}

func labels(rs []Reading) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Label
	}
	return out
}

func TestSummarizeCountsTheChipsThatAnswered(t *testing.T) {
	got := Summarize([]Reading{
		{Chip: "coretemp-isa-0000", Label: "Package id 0", Kind: SensorTemp, Value: 70, Crit: ptr(84)},
		{Chip: "coretemp-isa-0000", Label: "Core 0", Kind: SensorTemp, Value: 68, Crit: ptr(84)},
		{Chip: "nvme-pci-0100", Label: "Composite", Kind: SensorTemp, Value: 40, Crit: ptr(85)},
	})
	if got.Count != 3 {
		t.Errorf("count = %d, want 3", got.Count)
	}
	if len(got.Chips) != 2 {
		t.Errorf("chips = %v, want two", got.Chips)
	}
	if got.Hottest.Label != "Package id 0" {
		t.Errorf("hottest = %s, want the package", got.Hottest.Label)
	}
}

func TestShortChipTrimsTheBus(t *testing.T) {
	for in, want := range map[string]string{
		"coretemp-isa-0000": "coretemp",
		"nvme-pci-0100":     "nvme",
		"acpitz-acpi-0":     "acpitz",
		"k10temp-pci-00c3":  "k10temp",
		"plain":             "plain",
	} {
		if got := ShortChip(in); got != want {
			t.Errorf("ShortChip(%q) = %q, want %q", in, got, want)
		}
	}
}
