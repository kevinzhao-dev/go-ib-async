package protocol

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/kvzhao/go-ib-async/contract"
)

const (
	unsetDouble  float64 = math.MaxFloat64
	unsetInteger int64   = math.MaxInt32
)

// EncodeField converts a single value to its IB wire protocol string representation.
func EncodeField(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case int:
		return strconv.Itoa(val)
	case int64:
		if val == unsetInteger {
			return ""
		}
		return strconv.FormatInt(val, 10)
	case float64:
		if val == unsetDouble {
			return ""
		}
		if math.IsInf(val, 1) {
			return "Infinite"
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case bool:
		if val {
			return "1"
		}
		return "0"
	case []contract.TagValue:
		var sb strings.Builder
		for _, tv := range val {
			sb.WriteString(tv.Tag)
			sb.WriteByte('=')
			sb.WriteString(tv.Value)
			sb.WriteByte(';')
		}
		return sb.String()
	default:
		return fmt.Sprintf("%v", val)
	}
}

// EncodeContract serializes a Contract into its 12 null-delimited fields.
func EncodeContract(c *contract.Contract) string {
	fields := []string{
		strconv.FormatInt(c.ConID, 10),
		c.Symbol,
		c.SecType,
		c.LastTradeDateOrContractMonth,
		strconv.FormatFloat(c.Strike, 'f', -1, 64),
		c.Right,
		c.Multiplier,
		c.Exchange,
		c.PrimaryExchange,
		c.Currency,
		c.LocalSymbol,
		c.TradingClass,
	}
	return strings.Join(fields, "\x00")
}

// BuildMessage creates a null-delimited message from mixed fields.
// Contract pointers are expanded inline using EncodeContract.
func BuildMessage(fields ...interface{}) []byte {
	var sb strings.Builder
	for _, f := range fields {
		switch val := f.(type) {
		case *contract.Contract:
			sb.WriteString(EncodeContract(val))
		default:
			sb.WriteString(EncodeField(val))
		}
		sb.WriteByte('\x00')
	}
	return []byte(sb.String())
}
