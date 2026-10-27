package aggregate

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cpauk/model"
)

func TestCreditPricingPrecedenceAndRounding(t *testing.T) {
	credits := "0.000000025"
	event := model.Event{Provider: "kiro", Model: "m", ProviderCredits: &credits, Tokens: model.TokenUsage{Total: 100}}
	catalog := PricingRule{ID: "catalog", Model: "m", Catalog: true, Source: "catalog"}
	book := PriceBook{Rules: []PricingRule{catalog}}
	priced, err := book.Price(event)
	if err != nil || priced.KnownCost == nil || *priced.KnownCost != 1 || priced.PriceUSDPerCredit == nil || *priced.PriceUSDPerCredit != 20_000_000 || priced.Source != "kiro-credits" || priced.UnpricedTokens != 0 {
		t.Fatalf("credit price: %+v %v", priced, err)
	}
	credits = "0"
	priced, err = book.Price(event)
	if err != nil || priced.KnownCost == nil || *priced.KnownCost != 0 {
		t.Fatalf("zero: %+v %v", priced, err)
	}
	book.Rules = append(book.Rules, PricingRule{ID: "unknown", Model: "m", Source: "manual"})
	priced, err = book.Price(event)
	if err != nil || priced.KnownCost != nil || priced.UnpricedTokens != 100 {
		t.Fatalf("manual unknown: %+v %v", priced, err)
	}
	event.ProviderCredits = nil
	book.Rules = nil
	priced, err = book.Price(event)
	if err != nil || priced.KnownCost != nil {
		t.Fatalf("absent: %+v %v", priced, err)
	}
}
