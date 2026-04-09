package protocol

import (
	"math"
	"testing"

	"github.com/kevinzhao-dev/go-ib-async/contract"
)

// --- Fuzz Tests ---

func FuzzFieldReaderReadInt(f *testing.F) {
	f.Add("42")
	f.Add("")
	f.Add("-1")
	f.Add("999999999999")
	f.Add("abc")
	f.Add("1.5")

	f.Fuzz(func(t *testing.T, s string) {
		r := NewFieldReader([]string{s})
		_ = r.ReadInt() // must not panic
	})
}

func FuzzFieldReaderReadFloat(f *testing.F) {
	f.Add("42.5")
	f.Add("")
	f.Add("Infinite")
	f.Add("Infinity")
	f.Add("-1e308")
	f.Add("NaN")
	f.Add("abc")

	f.Fuzz(func(t *testing.T, s string) {
		r := NewFieldReader([]string{s})
		_ = r.ReadFloat() // must not panic
	})
}

func FuzzFieldReaderReadBool(f *testing.F) {
	f.Add("1")
	f.Add("0")
	f.Add("")
	f.Add("true")
	f.Add("false")
	f.Add("abc")

	f.Fuzz(func(t *testing.T, s string) {
		r := NewFieldReader([]string{s})
		_ = r.ReadBool() // must not panic
	})
}

func FuzzEncodeField(f *testing.F) {
	f.Add(42.5)
	f.Add(0.0)
	f.Add(-1.0)
	f.Add(math.MaxFloat64)
	f.Add(math.Inf(1))
	f.Add(math.NaN())

	f.Fuzz(func(t *testing.T, v float64) {
		result := EncodeField(v)
		// Must produce a string, never panic
		_ = result
	})
}

func FuzzEncodeContract(f *testing.F) {
	f.Add("AAPL", "STK", "SMART", "USD", 0.0, "", "")
	f.Add("", "", "", "", 100.5, "C", "100")
	f.Add("ES\x00evil", "FUT", "GLOBEX", "", 0.0, "", "")

	f.Fuzz(func(t *testing.T, symbol, secType, exchange, currency string, strike float64, right, multiplier string) {
		c := &contract.Contract{
			Symbol:     symbol,
			SecType:    secType,
			Exchange:   exchange,
			Currency:   currency,
			Strike:     strike,
			Right:      right,
			Multiplier: multiplier,
		}
		result := EncodeContract(c)
		_ = result // must not panic
	})
}

func FuzzDecodeMessage(f *testing.F) {
	// Seed with realistic message patterns
	f.Add("9\x001\x0042\x00")
	f.Add("4\x002\x001\x00321\x00Invalid\x00")
	f.Add("1\x006\x001\x001\x00150.5\x00100\x000\x00")
	f.Add("")
	f.Add("\x00\x00\x00")

	f.Fuzz(func(t *testing.T, msg string) {
		handler := func(msgID int, reader *FieldReader) {
			// Read all fields to exercise the reader
			for reader.HasMore() {
				_ = reader.ReadString()
			}
		}
		d := NewDecoder(178, handler)
		// Split on null like the real parser
		fields := splitFields(msg)
		if len(fields) > 0 {
			d.Interpret(fields)
		}
	})
}

func splitFields(msg string) []string {
	if msg == "" {
		return nil
	}
	var fields []string
	start := 0
	for i := 0; i < len(msg); i++ {
		if msg[i] == 0 {
			fields = append(fields, msg[start:i])
			start = i + 1
		}
	}
	if start < len(msg) {
		fields = append(fields, msg[start:])
	}
	return fields
}
