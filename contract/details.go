package contract

// ContractDetails contains full contract specification and market metadata.
type ContractDetails struct {
	Contract               *Contract
	MarketName             string
	MinTick                float64
	OrderTypes             string
	ValidExchanges         string
	PriceMagnifier         int
	UnderConID             int64
	LongName               string
	ContractMonth          string
	Industry               string
	Category               string
	Subcategory            string
	TimeZoneID             string
	TradingHours           string
	LiquidHours            string
	EvRule                 string
	EvMultiplier           int
	MdSizeMultiplier       int
	AggGroup               int
	UnderSymbol            string
	UnderSecType           string
	MarketRuleIDs          string
	SecIDList              []TagValue
	RealExpirationDate     string
	LastTradeTime          string
	StockType              string
	MinSize                float64
	SizeIncrement          float64
	SuggestedSizeIncrement float64
	// Bond fields
	CUSIP             string
	Ratings           string
	DescAppend        string
	BondType          string
	CouponType        string
	Callable          bool
	Putable           bool
	Coupon            float64
	Convertible       bool
	Maturity          string
	IssueDate         string
	NextOptionDate    string
	NextOptionType    string
	NextOptionPartial bool
	Notes             string
}

// NewContractDetails returns a ContractDetails with sensible defaults.
func NewContractDetails() *ContractDetails {
	return &ContractDetails{
		MdSizeMultiplier: 1,
	}
}

// ContractDescription is a symbol lookup result.
type ContractDescription struct {
	Contract           *Contract
	DerivativeSecTypes []string
}

// ScanData is a single scanner result row.
type ScanData struct {
	Rank            int
	ContractDetails *ContractDetails
	Distance        string
	Benchmark       string
	Projection      string
	LegsStr         string
}
