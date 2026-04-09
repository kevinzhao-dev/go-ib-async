package contract

import (
	"testing"
)

func TestStockConstructor(t *testing.T) {
	c := Stock("AAPL", "SMART", "USD")
	if c.SecType != "STK" {
		t.Fatalf("SecType = %q, want STK", c.SecType)
	}
	if c.Symbol != "AAPL" || c.Exchange != "SMART" || c.Currency != "USD" {
		t.Fatalf("unexpected fields: %+v", c)
	}
}

func TestOptionConstructor(t *testing.T) {
	c := Option("SPY", "20240315", 450, "C", "SMART")
	if c.SecType != "OPT" || c.Strike != 450 || c.Right != "C" {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestFutureConstructor(t *testing.T) {
	c := Future("ES", "20240315", "GLOBEX")
	if c.SecType != "FUT" || c.Symbol != "ES" || c.Exchange != "GLOBEX" {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestContFutureConstructor(t *testing.T) {
	c := ContFuture("ES", "GLOBEX")
	if c.SecType != "CONTFUT" {
		t.Fatalf("SecType = %q, want CONTFUT", c.SecType)
	}
}

func TestForexConstructor(t *testing.T) {
	c := Forex("EURUSD")
	if c.SecType != "CASH" {
		t.Fatalf("SecType = %q, want CASH", c.SecType)
	}
	if c.Symbol != "EUR" || c.Currency != "USD" {
		t.Fatalf("symbol=%q currency=%q, want EUR/USD", c.Symbol, c.Currency)
	}
	if c.Exchange != "IDEALPRO" {
		t.Fatalf("Exchange = %q, want IDEALPRO", c.Exchange)
	}
}

func TestForexInvalidPair(t *testing.T) {
	c := Forex("XYZ")
	if c.Symbol != "" {
		t.Fatalf("expected empty symbol for invalid pair, got %q", c.Symbol)
	}
}

func TestIndexConstructor(t *testing.T) {
	c := Index("SPX", "CBOE", "USD")
	if c.SecType != "IND" {
		t.Fatalf("SecType = %q, want IND", c.SecType)
	}
}

func TestCFDConstructor(t *testing.T) {
	c := CFD("IBUS30", "", "")
	if c.SecType != "CFD" {
		t.Fatalf("SecType = %q, want CFD", c.SecType)
	}
}

func TestCommodityConstructor(t *testing.T) {
	c := Commodity("XAUUSD", "SMART", "USD")
	if c.SecType != "CMDTY" {
		t.Fatalf("SecType = %q, want CMDTY", c.SecType)
	}
}

func TestBondConstructor(t *testing.T) {
	c := Bond()
	if c.SecType != "BOND" {
		t.Fatalf("SecType = %q, want BOND", c.SecType)
	}
}

func TestFuturesOptionConstructor(t *testing.T) {
	c := FuturesOption("ES", "20240315", 5000, "C", "GLOBEX")
	if c.SecType != "FOP" || c.Strike != 5000 {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestMutualFundConstructor(t *testing.T) {
	c := MutualFund()
	if c.SecType != "FUND" {
		t.Fatalf("SecType = %q, want FUND", c.SecType)
	}
}

func TestWarrantConstructor(t *testing.T) {
	c := Warrant()
	if c.SecType != "WAR" {
		t.Fatalf("SecType = %q, want WAR", c.SecType)
	}
}

func TestBagConstructor(t *testing.T) {
	c := Bag()
	if c.SecType != "BAG" {
		t.Fatalf("SecType = %q, want BAG", c.SecType)
	}
}

func TestCryptoConstructor(t *testing.T) {
	c := Crypto("BTC", "PAXOS", "USD")
	if c.SecType != "CRYPTO" || c.Symbol != "BTC" {
		t.Fatalf("unexpected: %+v", c)
	}
}

func TestContractEqualByConID(t *testing.T) {
	a := &Contract{ConID: 265598, Symbol: "AAPL"}
	b := &Contract{ConID: 265598, Symbol: "AAPL"}
	if !a.Equal(b) {
		t.Fatal("contracts with same ConID should be equal")
	}
}

func TestContractEqualByFields(t *testing.T) {
	a := Stock("AAPL", "SMART", "USD")
	b := Stock("AAPL", "SMART", "USD")
	if !a.Equal(b) {
		t.Fatal("contracts with same fields should be equal")
	}
}

func TestContractNotEqual(t *testing.T) {
	a := Stock("AAPL", "SMART", "USD")
	b := Stock("MSFT", "SMART", "USD")
	if a.Equal(b) {
		t.Fatal("different contracts should not be equal")
	}
}

func TestContractEqualNil(t *testing.T) {
	a := Stock("AAPL", "SMART", "USD")
	if a.Equal(nil) {
		t.Fatal("contract should not be equal to nil")
	}
}

func TestIsHashable(t *testing.T) {
	c := &Contract{ConID: 123}
	if !c.IsHashable() {
		t.Fatal("contract with ConID should be hashable")
	}
	c2 := &Contract{}
	if c2.IsHashable() {
		t.Fatal("contract without ConID should not be hashable")
	}
}

func TestKeyNormal(t *testing.T) {
	c := &Contract{ConID: 100, SecType: "STK"}
	if c.Key() != 100 {
		t.Fatalf("Key() = %d, want 100", c.Key())
	}
}

func TestKeyContFuture(t *testing.T) {
	c := &Contract{ConID: 100, SecType: "CONTFUT"}
	if c.Key() != -100 {
		t.Fatalf("Key() = %d, want -100", c.Key())
	}
}

func TestKeyBag(t *testing.T) {
	bag1 := &Contract{
		SecType: "BAG",
		Symbol:  "SPY",
		ComboLegs: []ComboLeg{
			{ConID: 100, Ratio: 1, Action: "BUY"},
			{ConID: 200, Ratio: 1, Action: "SELL"},
		},
	}
	bag2 := &Contract{
		SecType: "BAG",
		Symbol:  "SPY",
		ComboLegs: []ComboLeg{
			{ConID: 200, Ratio: 1, Action: "SELL"},
			{ConID: 100, Ratio: 1, Action: "BUY"},
		},
	}
	// Same legs in different order should produce same key
	if bag1.Key() != bag2.Key() {
		t.Fatalf("BAG keys should match: %d != %d", bag1.Key(), bag2.Key())
	}
}

func TestKeyBagDifferent(t *testing.T) {
	bag1 := &Contract{
		SecType:   "BAG",
		ComboLegs: []ComboLeg{{ConID: 100, Ratio: 1}},
	}
	bag2 := &Contract{
		SecType:   "BAG",
		ComboLegs: []ComboLeg{{ConID: 200, Ratio: 1}},
	}
	if bag1.Key() == bag2.Key() {
		t.Fatal("different BAG legs should produce different keys")
	}
}

func TestContractString(t *testing.T) {
	c := Stock("AAPL", "SMART", "USD")
	s := c.String()
	if s != "STK(AAPL, SMART, USD)" {
		t.Fatalf("String() = %q", s)
	}
}

func TestContractStringLocalSymbol(t *testing.T) {
	c := &Contract{SecType: "FUT", LocalSymbol: "ESH4", Exchange: "GLOBEX", Currency: "USD"}
	s := c.String()
	if s != "FUT(ESH4, GLOBEX, USD)" {
		t.Fatalf("String() = %q", s)
	}
}

func TestNewComboLeg(t *testing.T) {
	leg := NewComboLeg()
	if leg.ExemptCode != -1 {
		t.Fatalf("ExemptCode = %d, want -1", leg.ExemptCode)
	}
}

func TestNewContractDetails(t *testing.T) {
	cd := NewContractDetails()
	if cd.MdSizeMultiplier != 1 {
		t.Fatalf("MdSizeMultiplier = %d, want 1", cd.MdSizeMultiplier)
	}
}
