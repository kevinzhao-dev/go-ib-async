package protocol

import (
	"math"
	"strconv"
)

// FieldReader provides sequential typed reading of null-delimited message fields.
type FieldReader struct {
	Fields []string
	Pos    int
}

// NewFieldReader creates a FieldReader from a slice of fields.
func NewFieldReader(fields []string) *FieldReader {
	return &FieldReader{Fields: fields}
}

// HasMore returns true if there are more fields to read.
func (r *FieldReader) HasMore() bool {
	return r.Pos < len(r.Fields)
}

// ReadString reads the next field as a string.
func (r *FieldReader) ReadString() string {
	if r.Pos >= len(r.Fields) {
		return ""
	}
	v := r.Fields[r.Pos]
	r.Pos++
	return v
}

// ReadInt reads the next field as an int64. Returns 0 for empty strings.
func (r *FieldReader) ReadInt() int64 {
	s := r.ReadString()
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// ReadFloat reads the next field as a float64. Returns 0 for empty strings.
func (r *FieldReader) ReadFloat() float64 {
	s := r.ReadString()
	if s == "" {
		return 0
	}
	if s == "Infinite" || s == "Infinity" {
		return math.Inf(1)
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// ReadBool reads the next field as a bool. "1" or "true" → true.
func (r *FieldReader) ReadBool() bool {
	s := r.ReadString()
	return s == "1" || s == "true"
}

// ReadIntList reads the next N fields as int64s.
func (r *FieldReader) ReadIntList(n int) []int64 {
	result := make([]int64, n)
	for i := range n {
		result[i] = r.ReadInt()
	}
	return result
}

// Skip advances the position by n fields.
func (r *FieldReader) Skip(n int) {
	r.Pos += n
	if r.Pos > len(r.Fields) {
		r.Pos = len(r.Fields)
	}
}
