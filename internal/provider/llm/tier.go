package llm

// Tier is what modules request; configuration maps it to a concrete model.
type Tier string

const (
	TierFast      Tier = "fast"
	TierReasoning Tier = "reasoning"
	TierCheap     Tier = "cheap"
)

func Tiers() []Tier { return []Tier{TierFast, TierReasoning, TierCheap} }

func (t Tier) Valid() bool {
	switch t {
	case TierFast, TierReasoning, TierCheap:
		return true
	}
	return false
}

// Target is a resolved tier: a client plus the model to call on it.
type Target struct {
	LLM           LLM
	Model         string
	NoTemperature bool
	Params        map[string]any
}

type Resolver interface {
	Resolve(t Tier) (Target, error)
}
