package ibgo

import (
	"math"
	"testing"
)

func TestConstants(t *testing.T) {
	if UnsetInteger != math.MaxInt32 {
		t.Fatalf("UnsetInteger = %d, want %d", UnsetInteger, int64(math.MaxInt32))
	}
	if UnsetDouble != math.MaxFloat64 {
		t.Fatalf("UnsetDouble = %g, want %g", UnsetDouble, math.MaxFloat64)
	}
	if MinClientVersion != 157 {
		t.Fatalf("MinClientVersion = %d, want 157", MinClientVersion)
	}
	if MaxClientVersion != 178 {
		t.Fatalf("MaxClientVersion = %d, want 178", MaxClientVersion)
	}
}

func TestEpoch(t *testing.T) {
	if Epoch.Year() != 1970 || Epoch.Month() != 1 || Epoch.Day() != 1 {
		t.Fatalf("Epoch = %v, want 1970-01-01", Epoch)
	}
}
