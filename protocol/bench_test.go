package protocol

import (
	"strings"
	"testing"

	"github.com/kevinzhao-dev/go-ib-async/contract"
)

func BenchmarkEncodeField_Float(b *testing.B) {
	for b.Loop() {
		EncodeField(150.5)
	}
}

func BenchmarkEncodeField_Int64(b *testing.B) {
	for b.Loop() {
		EncodeField(int64(42))
	}
}

func BenchmarkEncodeField_Bool(b *testing.B) {
	for b.Loop() {
		EncodeField(true)
	}
}

func BenchmarkEncodeContract(b *testing.B) {
	c := contract.Stock("AAPL", "SMART", "USD")
	c.ConID = 265598
	for b.Loop() {
		EncodeContract(c)
	}
}

func BenchmarkBuildMessage(b *testing.B) {
	c := contract.Stock("AAPL", "SMART", "USD")
	c.ConID = 265598
	for b.Loop() {
		BuildMessage(1, 11, int64(42), c, true, 150.5)
	}
}

func BenchmarkFieldReaderParse(b *testing.B) {
	fields := []string{"1", "6", "42", "1", "150.50", "100", "0"}
	for b.Loop() {
		r := NewFieldReader(fields)
		_ = r.ReadInt()
		_ = r.ReadInt()
		_ = r.ReadInt()
		_ = r.ReadBool()
		_ = r.ReadFloat()
		_ = r.ReadFloat()
		_ = r.ReadBool()
	}
}

func BenchmarkDecoderInterpret(b *testing.B) {
	handler := func(msgID int, reader *FieldReader) {
		_ = reader.ReadInt()
		_ = reader.ReadInt()
		_ = reader.ReadFloat()
		_ = reader.ReadFloat()
	}
	d := NewDecoder(178, handler)
	fields := []string{"1", "6", "42", "1", "150.50", "100"}

	for b.Loop() {
		d.Interpret(fields)
	}
}

func BenchmarkMessageFraming(b *testing.B) {
	// Simulate building a full framed message
	c := contract.Stock("AAPL", "SMART", "USD")
	c.ConID = 265598
	for b.Loop() {
		msg := BuildMessage(1, 11, int64(42), c, "", true, false, nil)
		_ = msg
	}
}

func BenchmarkSplitNullDelimited(b *testing.B) {
	msg := "9\x001\x0042\x00something\x00more\x00fields\x00here\x00"
	for b.Loop() {
		fields := strings.Split(msg, "\x00")
		if len(fields) > 0 && fields[len(fields)-1] == "" {
			fields = fields[:len(fields)-1]
		}
		_ = fields
	}
}
