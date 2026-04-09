package order

// OrderCondition is the interface for all order condition types.
type OrderCondition interface {
	CondType() int
	GetConjunction() string
	SetConjunction(string)
}

// CreateCondition returns a new condition of the given type.
func CreateCondition(condType int) OrderCondition {
	switch condType {
	case 1:
		return &PriceCondition{condType: 1, Conjunction: "a"}
	case 3:
		return &TimeCondition{condType: 3, Conjunction: "a"}
	case 4:
		return &MarginCondition{condType: 4, Conjunction: "a"}
	case 5:
		return &ExecutionCondition{condType: 5, Conjunction: "a"}
	case 6:
		return &VolumeCondition{condType: 6, Conjunction: "a"}
	case 7:
		return &PercentChangeCondition{condType: 7, Conjunction: "a"}
	default:
		return nil
	}
}

// PriceCondition triggers when a price threshold is reached.
type PriceCondition struct {
	condType      int
	Conjunction   string
	IsMore        bool
	Price         float64
	ConID         int64
	Exchange      string
	TriggerMethod int
}

func (c *PriceCondition) CondType() int              { return c.condType }
func (c *PriceCondition) GetConjunction() string      { return c.Conjunction }
func (c *PriceCondition) SetConjunction(s string)     { c.Conjunction = s }

// TimeCondition triggers at a specific time.
type TimeCondition struct {
	condType    int
	Conjunction string
	IsMore      bool
	Time        string
}

func (c *TimeCondition) CondType() int              { return c.condType }
func (c *TimeCondition) GetConjunction() string      { return c.Conjunction }
func (c *TimeCondition) SetConjunction(s string)     { c.Conjunction = s }

// MarginCondition triggers when margin level changes.
type MarginCondition struct {
	condType    int
	Conjunction string
	IsMore      bool
	Percent     int
}

func (c *MarginCondition) CondType() int              { return c.condType }
func (c *MarginCondition) GetConjunction() string      { return c.Conjunction }
func (c *MarginCondition) SetConjunction(s string)     { c.Conjunction = s }

// ExecutionCondition triggers after an execution in another contract.
type ExecutionCondition struct {
	condType    int
	Conjunction string
	SecType     string
	Exchange    string
	Symbol      string
}

func (c *ExecutionCondition) CondType() int              { return c.condType }
func (c *ExecutionCondition) GetConjunction() string      { return c.Conjunction }
func (c *ExecutionCondition) SetConjunction(s string)     { c.Conjunction = s }

// VolumeCondition triggers when volume reaches a threshold.
type VolumeCondition struct {
	condType    int
	Conjunction string
	IsMore      bool
	Volume      int
	ConID       int64
	Exchange    string
}

func (c *VolumeCondition) CondType() int              { return c.condType }
func (c *VolumeCondition) GetConjunction() string      { return c.Conjunction }
func (c *VolumeCondition) SetConjunction(s string)     { c.Conjunction = s }

// PercentChangeCondition triggers on percent change.
type PercentChangeCondition struct {
	condType      int
	Conjunction   string
	IsMore        bool
	ChangePercent float64
	ConID         int64
	Exchange      string
}

func (c *PercentChangeCondition) CondType() int              { return c.condType }
func (c *PercentChangeCondition) GetConjunction() string      { return c.Conjunction }
func (c *PercentChangeCondition) SetConjunction(s string)     { c.Conjunction = s }
