package notifications

import (
	"encoding/json"
	"regexp"
	"slices"
)

// The embedded provider contract is the single source of allowed delivery titles.
type titleRule struct {
	Type        string   `json:"type"`
	TargetKinds []string `json:"target_kinds"`
	Parameters  []string `json:"parameters"`
}

var titleCatalog = func() map[string]titleRule {
	var contract struct {
		Catalog map[string]titleRule `json:"x-title-catalog"`
	}
	if err := json.Unmarshal(openAPI, &contract); err != nil {
		panic(err)
	}
	if len(contract.Catalog) == 0 {
		panic("missing notification title catalog")
	}
	return contract.Catalog
}()

// Only opaque business references are accepted, never arbitrary user text.
// Producers must not use personal identifiers even if they match this syntax.
var displayReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

func validateTitle(d Event) error {
	rule, ok := titleCatalog[d.TitleKey]
	if !ok || d.Type != rule.Type || !slices.Contains(rule.TargetKinds, d.Target.Kind) || len(d.TitleParams) != len(rule.Parameters) {
		return ErrInput
	}
	for _, name := range rule.Parameters {
		if !displayReference.MatchString(d.TitleParams[name]) {
			return ErrInput
		}
	}
	return nil
}
