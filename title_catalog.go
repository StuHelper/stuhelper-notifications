package notifications

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

// The embedded provider contract is the single source of allowed delivery titles.
type titleRule struct {
	ReadParameters     []string `json:"read_parameters"`
	OptionalParameters []string `json:"optional_parameters"`
	Type               string   `json:"type"`
	TargetKinds        []string `json:"target_kinds"`
	Parameters         []string `json:"parameters"`
}

var titleCatalog, allowedReasonCodes = func() (map[string]titleRule, map[string]bool) {
	var contract struct {
		Catalog    map[string]titleRule `json:"x-title-catalog"`
		Components struct {
			Schemas struct {
				ReasonCode struct {
					Enum []string `json:"enum"`
				} `json:"ReasonCode"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPI, &contract); err != nil {
		panic(err)
	}
	if len(contract.Catalog) == 0 || len(contract.Components.Schemas.ReasonCode.Enum) == 0 {
		panic("missing notification title catalog or reason codes")
	}
	reasons := make(map[string]bool, len(contract.Components.Schemas.ReasonCode.Enum))
	for _, code := range contract.Components.Schemas.ReasonCode.Enum {
		reasons[code] = true
	}
	return contract.Catalog, reasons
}()

// Only opaque business references are accepted, never arbitrary user text.
// Producers must not use personal identifiers even if they match this syntax.
var displayReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

func validateTitle(d Event) error {
	rule, ok := titleCatalog[d.TitleKey]
	if !ok || d.Type != rule.Type || !slices.Contains(rule.TargetKinds, d.Target.Kind) {
		return ErrInput
	}
	if len(d.TitleParams) < len(rule.Parameters) || len(d.TitleParams) > len(rule.Parameters)+len(rule.OptionalParameters) {
		return ErrInput
	}
	for _, name := range rule.Parameters {
		val, exists := d.TitleParams[name]
		if !exists || !displayReference.MatchString(val) {
			return ErrInput
		}
	}
	for name, val := range d.TitleParams {
		if slices.Contains(rule.Parameters, name) {
			continue
		}
		if !slices.Contains(rule.OptionalParameters, name) {
			return ErrInput
		}
		if name == "reason_code" {
			if !allowedReasonCodes[val] {
				return ErrInput
			}
			continue
		}
		if !displayReference.MatchString(val) {
			return ErrInput
		}
	}
	return nil
}

// BoxForTitleKey maps a title catalog key to its message-center box.
func BoxForTitleKey(titleKey string) string {
	subject := strings.TrimPrefix(titleKey, "notifications.")
	if i := strings.IndexByte(subject, '.'); i >= 0 {
		subject = subject[:i]
	}
	switch {
	case subject == "interaction":
		return BoxInteraction
	case subject == "content" || subject == "resource_removal" || subject == "report" || subject == "appeal" || subject == "teacher_homepage_request":
		return BoxReview
	case strings.HasSuffix(subject, "_verification") || strings.HasPrefix(subject, "verification_"):
		return BoxVerification
	default:
		return BoxSystem
	}
}

func validBox(box string) bool {
	switch box {
	case BoxInteraction, BoxReview, BoxVerification, BoxSystem:
		return true
	default:
		return false
	}
}

func validNotificationType(kind string) bool {
	switch kind {
	case "interaction", "business", "security", "account":
		return true
	default:
		return false
	}
}
