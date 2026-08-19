package sensors

import (
	"strings"
	"testing"

	"github.com/freezxp/proxui/internal/domain/telemetry"
)

// Real output, trimmed: an Intel package with per-core readings, an NVMe with
// a composite and two probes, and an ACPI zone with no thresholds at all.
const realOutput = `{
   "coretemp-isa-0000":{
      "Adapter": "ISA adapter",
      "Package id 0":{ "temp1_input": 47.000, "temp1_max": 84.000, "temp1_crit": 100.000, "temp1_crit_alarm": 0.000 },
      "Core 0":{ "temp2_input": 45.000, "temp2_max": 84.000, "temp2_crit": 100.000 },
      "Core 1":{ "temp3_input": 46.000, "temp3_max": 84.000, "temp3_crit": 100.000 }
   },
   "nvme-pci-0100":{
      "Adapter": "PCI adapter",
      "Composite":{ "temp1_input": 38.850, "temp1_max": 81.850, "temp1_crit": 84.850, "temp1_alarm": 0.000 },
      "Sensor 1":{ "temp2_input": 38.850 },
      "Sensor 2":{ "temp3_input": 46.850 }
   },
   "acpitz-acpi-0":{
      "Adapter": "ACPI interface",
      "temp1":{ "temp1_input": 27.800 }
   }
}`

func find(t *testing.T, readings []telemetry.Reading, chip, label string) telemetry.Reading {
	t.Helper()
	for _, r := range readings {
		if r.Chip == chip && r.Label == label {
			return r
		}
	}
	t.Fatalf("no reading for %s / %s", chip, label)
	return telemetry.Reading{}
}

func TestParseRealOutput(t *testing.T) {
	readings, err := Parse([]byte(realOutput))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(readings) != 7 {
		t.Fatalf("got %d readings, want 7: %+v", len(readings), readings)
	}

	pkg := find(t, readings, "coretemp-isa-0000", "Package id 0")
	if pkg.Value != 47 || pkg.Kind != telemetry.SensorTemp {
		t.Errorf("package = %+v, want 47°C", pkg)
	}
	// The thresholds travel with the reading, because the chip is the only
	// thing that knows what hot means for it.
	if pkg.High == nil || *pkg.High != 84 || pkg.Crit == nil || *pkg.Crit != 100 {
		t.Errorf("package thresholds = %v/%v, want 84/100", pkg.High, pkg.Crit)
	}

	// A chip that declares no thresholds still reports its reading.
	zone := find(t, readings, "acpitz-acpi-0", "temp1")
	if zone.Value != 27.8 || zone.Crit != nil {
		t.Errorf("acpi zone = %+v, want 27.8°C and no critical point", zone)
	}
}

// The label under a feature can hold more than one numbered set, and the digit
// is what ties a reading to its own thresholds rather than the next one's.
func TestNumberedSetsDoNotBlend(t *testing.T) {
	readings, err := Parse([]byte(`{"chip":{"Both":{
		"temp1_input": 40.0, "temp1_crit": 90.0,
		"temp2_input": 60.0, "temp2_crit": 70.0}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 2 {
		t.Fatalf("got %d readings, want 2", len(readings))
	}
	for _, r := range readings {
		switch r.Value {
		case 40:
			if *r.Crit != 90 {
				t.Errorf("40°C reading took the other set's critical point: %v", *r.Crit)
			}
		case 60:
			if *r.Crit != 70 {
				t.Errorf("60°C reading took the other set's critical point: %v", *r.Crit)
			}
		default:
			t.Errorf("unexpected reading %v", r.Value)
		}
	}
}

func TestParseSkipsWhatItCannotTrust(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		// A driver reporting a fault is reporting that its number is wrong.
		{"a faulted probe", `{"c":{"f":{"temp1_input": 55.0, "temp1_fault": 1.0}}}`},
		// Disconnected probes read as exactly zero on several drivers.
		{"a disconnected probe", `{"c":{"f":{"temp1_input": 0.0}}}`},
		// A threshold with no reading beside it describes nothing.
		{"thresholds with no reading", `{"c":{"f":{"temp1_crit": 100.0}}}`},
		// Voltages and everything else are not collected today.
		{"a voltage", `{"c":{"f":{"in0_input": 1.2}}}`},
		{"the adapter line", `{"c":{"Adapter": "ISA adapter"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readings, err := Parse([]byte(tt.in))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(readings) != 0 {
				t.Errorf("got %+v, want nothing", readings)
			}
		})
	}
}

// One unreadable chip must not cost the readings on every other chip: boards
// carry drivers nobody testing this had.
func TestOneOddChipDoesNotCostTheRest(t *testing.T) {
	readings, err := Parse([]byte(`{
		"good":{"Package id 0":{"temp1_input": 50.0}},
		"odd":{"Weird":{"temp1_input": "not a number"}},
		"flat": {"Adapter": "ISA adapter"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(readings) != 1 || readings[0].Chip != "good" {
		t.Errorf("got %+v, want just the good chip", readings)
	}
}

// A node without lm-sensors answers with nothing on stdout, and that has to
// read as "not set up" rather than as a parse failure nobody can action.
func TestMissingSensorsIsNamed(t *testing.T) {
	_, err := Parse([]byte("   \n"))
	if err == nil {
		t.Fatal("empty output was accepted")
	}
	if !strings.Contains(err.Error(), "lm-sensors") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

func TestGarbageIsRejected(t *testing.T) {
	if _, err := Parse([]byte("bash: sensors: command not found")); err == nil {
		t.Error("a shell error message parsed as sensor output")
	}
}
