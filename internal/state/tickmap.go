package state

// PriceTickMap maps tick type IDs to Ticker price field names.
var PriceTickMap = map[int]string{
	6: "High", 72: "High",
	7: "Low", 73: "Low",
	9: "Close", 75: "Close",
	14: "Open", 76: "Open",
	15: "Low13Week", 16: "High13Week",
	17: "Low26Week", 18: "High26Week",
	19: "Low52Week", 20: "High52Week",
	35: "AuctionPrice",
	37: "MarkPrice",
	50: "BidYield", 103: "BidYield",
	51: "AskYield", 104: "AskYield",
	52: "LastYield",
	57: "LastRthTrade",
	78: "CreditmanMarkPrice",
	79: "CreditmanSlowMarkPrice",
	92: "EtfNavClose", 93: "EtfNavPriorClose",
	94: "EtfNavBid", 95: "EtfNavAsk",
	96: "EtfNavLast", 97: "EtfFrozenNavLast",
	98: "EtfNavHigh", 99: "EtfNavLow",
	101: "EstimatedIpoMidpoint",
	102: "FinalIpoLast",
}

// SizeTickMap maps tick type IDs to Ticker size field names.
var SizeTickMap = map[int]string{
	8: "Volume", 74: "Volume",
	63: "VolumeRate3Min", 64: "VolumeRate5Min", 65: "VolumeRate10Min",
	21: "AvVolume",
	22: "OpenInterest",
	27: "CallOpenInterest", 28: "PutOpenInterest",
	29: "CallVolume", 30: "PutVolume",
	34: "AuctionVolume",
	36: "AuctionImbalance",
	61: "RegulatoryImbalance",
	86: "FuturesOpenInterest",
	87: "AvOptionVolume",
	89: "ShortableShares",
}

// GenericTickMap maps tick type IDs to Ticker generic float field names.
var GenericTickMap = map[int]string{
	23: "HistVolatility",
	24: "ImpliedVolatility",
	31: "IndexFuturePremium",
	46: "Shortable",
	49: "Halted",
	54: "TradeCount",
	55: "TradeRate",
	56: "VolumeRate",
	58: "RTHistVolatility",
	60: "BondFactorMultiplier",
	90: "DelayedHalted",
}

// GreeksTickMap maps tick type IDs to Ticker greeks field names.
var GreeksTickMap = map[int]string{
	10: "BidGreeks", 80: "BidGreeks",
	11: "AskGreeks", 81: "AskGreeks",
	12: "LastGreeks", 82: "LastGreeks",
	13: "ModelGreeks", 83: "ModelGreeks",
	53: "CustGreeks",
}

// EfpTickMap maps tick type IDs to Ticker EFP field names.
var EfpTickMap = map[int]string{
	38: "BidEfp", 39: "AskEfp",
	40: "LastEfp", 41: "OpenEfp",
	42: "HighEfp", 43: "LowEfp",
	44: "CloseEfp",
}

// StringTickMap maps tick type IDs to Ticker string field names.
var StringTickMap = map[int]string{
	25: "OptionBidExch", 26: "OptionAskExch",
	32: "BidExchange", 33: "AskExchange",
	84: "LastExchange", 85: "LastRegTime",
	91: "ReutersMutualFunds",
	100: "SocialMarketAnalytics",
}

// TimestampTickMap maps tick type IDs to Ticker timestamp field names.
var TimestampTickMap = map[int]string{
	45: "LastTimestamp",
	88: "DelayedLastTimestamp",
}

// RTVolumeTickMap maps tick type IDs to Ticker RT volume field names.
var RTVolumeTickMap = map[int]string{
	48: "RTVolume",
	77: "RTTradeVolume",
}

// AllTickTypes returns all known tick type IDs.
func AllTickTypes() []int {
	seen := make(map[int]bool)
	maps := []map[int]string{
		PriceTickMap, SizeTickMap, GenericTickMap,
		GreeksTickMap, EfpTickMap, StringTickMap,
		TimestampTickMap, RTVolumeTickMap,
	}
	var result []int
	for _, m := range maps {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				result = append(result, k)
			}
		}
	}
	return result
}
