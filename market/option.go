package market

// OptionComputation holds Greeks and option pricing data.
type OptionComputation struct {
	TickAttrib int
	ImpliedVol *float64
	Delta      *float64
	OptPrice   *float64
	PvDividend *float64
	Gamma      *float64
	Vega       *float64
	Theta      *float64
	UndPrice   *float64
}

// Add returns the sum of two OptionComputations.
func (o *OptionComputation) Add(other *OptionComputation) *OptionComputation {
	return &OptionComputation{
		ImpliedVol: addOptFloat(o.ImpliedVol, other.ImpliedVol),
		Delta:      addOptFloat(o.Delta, other.Delta),
		OptPrice:   addOptFloat(o.OptPrice, other.OptPrice),
		Gamma:      addOptFloat(o.Gamma, other.Gamma),
		Vega:       addOptFloat(o.Vega, other.Vega),
		Theta:      addOptFloat(o.Theta, other.Theta),
		UndPrice:   o.UndPrice,
	}
}

// Sub returns the difference of two OptionComputations.
func (o *OptionComputation) Sub(other *OptionComputation) *OptionComputation {
	return &OptionComputation{
		ImpliedVol: subOptFloat(o.ImpliedVol, other.ImpliedVol),
		Delta:      subOptFloat(o.Delta, other.Delta),
		OptPrice:   subOptFloat(o.OptPrice, other.OptPrice),
		Gamma:      subOptFloat(o.Gamma, other.Gamma),
		Vega:       subOptFloat(o.Vega, other.Vega),
		Theta:      subOptFloat(o.Theta, other.Theta),
		UndPrice:   o.UndPrice,
	}
}

// Mul returns the OptionComputation scaled by a factor.
func (o *OptionComputation) Mul(factor float64) *OptionComputation {
	return &OptionComputation{
		ImpliedVol: mulOptFloat(o.ImpliedVol, factor),
		Delta:      mulOptFloat(o.Delta, factor),
		OptPrice:   mulOptFloat(o.OptPrice, factor),
		Gamma:      mulOptFloat(o.Gamma, factor),
		Vega:       mulOptFloat(o.Vega, factor),
		Theta:      mulOptFloat(o.Theta, factor),
		UndPrice:   o.UndPrice,
	}
}

func addOptFloat(a, b *float64) *float64 {
	va, vb := optVal(a), optVal(b)
	r := va + vb
	return &r
}

func subOptFloat(a, b *float64) *float64 {
	va, vb := optVal(a), optVal(b)
	r := va - vb
	return &r
}

func mulOptFloat(a *float64, factor float64) *float64 {
	r := optVal(a) * factor
	return &r
}

func optVal(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// Float64Ptr is a helper to create a *float64.
func Float64Ptr(v float64) *float64 {
	return &v
}

// OptionChain holds available option strikes and expirations.
type OptionChain struct {
	Exchange        string
	UnderlyingConID int64
	TradingClass    string
	Multiplier      string
	Expirations     []string
	Strikes         []float64
}

// EfpData holds Exchange for Physical futures pricing data.
type EfpData struct {
	BasisPoints              float64
	FormattedBasisPoints     string
	ImpliedFuture            float64
	HoldDays                 int
	FutureLastTradeDate      string
	DividendImpact           float64
	DividendsToLastTradeDate float64
}

// Dividends holds dividend information.
type Dividends struct {
	Past12Months *float64
	Next12Months *float64
	NextDate     string
	NextAmount   *float64
}
