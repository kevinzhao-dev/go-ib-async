package ibgo

import (
	"math"
	"time"
)

const (
	// UnsetInteger is the IB sentinel value for "no integer value set".
	UnsetInteger int64 = math.MaxInt32 // 2^31 - 1 = 2147483647

	// UnsetDouble is the IB sentinel value for "no float value set".
	UnsetDouble float64 = math.MaxFloat64

	// MinClientVersion is the minimum TWS API version supported.
	MinClientVersion = 157

	// MaxClientVersion is the maximum TWS API version supported.
	MaxClientVersion = 178

	// MaxRequests is the default rate limit (requests per interval).
	MaxRequests = 45

	// RequestsInterval is the default rate limit window.
	RequestsInterval = 1 * time.Second
)

// Epoch is the Unix epoch used as default zero time.
var Epoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
