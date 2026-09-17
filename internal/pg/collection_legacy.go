package pg

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

type legacyCollectionLine struct {
	ProviderLineID, QuoteLineID, ProviderPriceID string
	Quantity                                     int64
}

type legacyCollectionInput struct {
	Account                                               billing.AccountID
	Scope                                                 billing.Scope
	TransactionID, IntentID, QuoteFingerprint, CustomerID string
	Lines                                                 []legacyCollectionLine
	Actor, Reason, EvidenceReference                      string
}

type legacyCollectionBinding struct {
	legacyCollectionInput
	CreatedAt time.Time
}

func legacyCollectionFingerprint(in legacyCollectionBinding) string {
	in.Lines = slices.Clone(in.Lines)
	slices.SortFunc(in.Lines, func(a, b legacyCollectionLine) int { return cmp.Compare(a.ProviderLineID, b.ProviderLineID) })
	in.CreatedAt = billing.CanonicalTime(in.CreatedAt)
	raw, err := json.Marshal(in)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (in legacyCollectionBinding) validate() error {
	digest, digestErr := hex.DecodeString(in.QuoteFingerprint)
	if !billing.ValidID(string(in.Account)) || !in.Scope.Valid() || !billing.ValidID(in.TransactionID) || !billing.ValidID(in.IntentID) || digestErr != nil || len(digest) != sha256.Size || !billing.ValidID(in.Actor) || !billing.ValidID(in.Reason) || !billing.ValidID(in.EvidenceReference) || len(in.Lines) == 0 || len(in.Lines) > 100 || in.CreatedAt.IsZero() {
		return billing.ErrInvalid
	}
	if in.CustomerID != "" && !billing.ValidID(in.CustomerID) {
		return billing.ErrInvalid
	}
	providerIDs, quoteIDs := make(map[string]bool, len(in.Lines)), make(map[string]bool, len(in.Lines))
	for _, line := range in.Lines {
		if !billing.ValidID(line.ProviderLineID) || !billing.ValidID(line.QuoteLineID) || line.Quantity <= 0 || line.ProviderPriceID != "" && !billing.ValidID(line.ProviderPriceID) || providerIDs[line.ProviderLineID] || quoteIDs[line.QuoteLineID] {
			return billing.ErrInvalid
		}
		providerIDs[line.ProviderLineID], quoteIDs[line.QuoteLineID] = true, true
	}
	return nil
}

func (in legacyCollectionBinding) current() purchase.CollectionBinding {
	lines := make([]purchase.CollectionLine, len(in.Lines))
	for i, line := range in.Lines {
		lines[i] = purchase.CollectionLine{
			ProviderLineID: line.ProviderLineID, ProviderPriceID: line.ProviderPriceID, Quantity: line.Quantity,
			Allocations: []purchase.CollectionAllocation{{QuoteLineID: line.QuoteLineID, Quantity: line.Quantity}},
		}
	}
	return purchase.CollectionBinding{CollectionInput: purchase.CollectionInput{
		Account: in.Account, Scope: in.Scope, TransactionID: in.TransactionID, IntentID: in.IntentID,
		QuoteFingerprint: in.QuoteFingerprint, CustomerID: in.CustomerID, Lines: lines,
		Actor: in.Actor, Reason: in.Reason, EvidenceReference: in.EvidenceReference,
	}, CreatedAt: in.CreatedAt}
}

var collectionBindingKeys = map[string]bool{
	"Account": true, "Scope": true, "TransactionID": true, "IntentID": true, "QuoteFingerprint": true,
	"CustomerID": true, "Lines": true, "Actor": true, "Reason": true, "EvidenceReference": true, "CreatedAt": true,
}

func collectionBindingShape(raw []byte) (bool, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || !exactJSONKeys(object, collectionBindingKeys) {
		return false, billing.ErrInvalid
	}
	var scope map[string]json.RawMessage
	if err := json.Unmarshal(object["Scope"], &scope); err != nil || !exactJSONKeys(scope, map[string]bool{"Provider": true, "Merchant": true, "Environment": true}) {
		return false, billing.ErrInvalid
	}
	var lines []map[string]json.RawMessage
	if err := json.Unmarshal(object["Lines"], &lines); err != nil || len(lines) == 0 {
		return false, billing.ErrInvalid
	}
	legacy := false
	for i, line := range lines {
		_, old := line["QuoteLineID"]
		_, current := line["Allocations"]
		if old == current {
			return false, billing.ErrInvalid
		}
		if i == 0 {
			legacy = old
		} else if legacy != old {
			return false, billing.ErrInvalid
		}
		if old {
			if !exactJSONKeys(line, map[string]bool{"ProviderLineID": true, "QuoteLineID": true, "ProviderPriceID": true, "Quantity": true}) {
				return false, billing.ErrInvalid
			}
			continue
		}
		if !exactJSONKeys(line, map[string]bool{"ProviderLineID": true, "ProviderPriceID": true, "Quantity": true, "Allocations": true}) {
			return false, billing.ErrInvalid
		}
		var allocations []map[string]json.RawMessage
		if err := json.Unmarshal(line["Allocations"], &allocations); err != nil || len(allocations) == 0 {
			return false, billing.ErrInvalid
		}
		for _, allocation := range allocations {
			if !exactJSONKeys(allocation, map[string]bool{"QuoteLineID": true, "Quantity": true}) {
				return false, billing.ErrInvalid
			}
		}
	}
	return legacy, nil
}

func exactJSONKeys(object map[string]json.RawMessage, keys map[string]bool) bool {
	if len(object) != len(keys) {
		return false
	}
	for key := range object {
		if !keys[key] {
			return false
		}
	}
	return true
}
