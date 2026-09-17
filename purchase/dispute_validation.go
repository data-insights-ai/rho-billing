package purchase

import (
	"encoding/json"
	"slices"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

const maxDisputeLines = 100

func validDisputeStatus(status DisputeStatus) bool {
	switch status {
	case DisputeWarning, DisputeOpen, DisputeUnderReview, DisputeLost, DisputeWon, DisputeClosed:
		return true
	}
	return false
}
func validateDisputeLines(lines []PaidLine, allowEmpty bool) error {
	if !allowEmpty && len(lines) == 0 {
		return billing.ErrInvalid
	}
	if len(lines) > maxDisputeLines {
		return billing.ErrInvalid
	}
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		if !billing.ValidID(line.LineID) || line.Gross <= 0 || line.Tax < 0 || line.Tax > line.Gross {
			return billing.ErrInvalid
		}
		if _, ok := seen[line.LineID]; ok {
			return billing.ErrConflict
		}
		seen[line.LineID] = struct{}{}
	}
	return nil
}
func sortedDisputeLines(lines []PaidLine) []PaidLine {
	out := slices.Clone(lines)
	slices.SortFunc(out, func(a, b PaidLine) int {
		if a.LineID < b.LineID {
			return -1
		}
		if a.LineID > b.LineID {
			return 1
		}
		return 0
	})
	return out
}
func validDisputeIDFields(account billing.AccountID, scope billing.Scope, ids ...string) bool {
	if !billing.ValidID(string(account)) || !scope.Valid() {
		return false
	}
	for _, id := range ids {
		if !billing.ValidID(id) {
			return false
		}
	}
	return true
}

func (f DisputeFact) Validate() error {
	if !validDisputeIDFields(f.Account, f.Scope, f.EventID, f.DisputeID, f.IntentID, f.TransactionID) || !validCurrency(f.Currency) || f.Amount <= 0 || !validDisputeStatus(f.Status) || f.OccurredAt.IsZero() || !billing.ValidID(f.EvidenceReference) || len(f.EvidenceReference) > 2000 {
		return billing.ErrInvalid
	}
	if f.DebitAdjustmentID != "" && !billing.ValidID(f.DebitAdjustmentID) {
		return billing.ErrInvalid
	}
	if _, err := f.OccurredAt.MarshalJSON(); err != nil {
		return billing.ErrInvalid
	}
	if f.Recovery != nil {
		if f.DebitAdjustmentID != "" && f.DebitAdjustmentID != f.Recovery.AdjustmentID {
			return billing.ErrConflict
		}
		if err := f.Recovery.Validate(); err != nil {
			return err
		}
	}
	return nil
}
func (f DisputeFact) Fingerprint() string {
	f.OccurredAt = billing.CanonicalTime(f.OccurredAt)
	if f.Recovery != nil {
		r := *f.Recovery
		r.Lines = sortedDisputeLines(r.Lines)
		f.Recovery = &r
	}
	return digest(f)
}
func (r DisputeRecovery) Validate() error {
	if !billing.ValidID(r.ID) || !billing.ValidID(r.AdjustmentID) || !billing.ValidID(r.EvidenceReference) {
		return billing.ErrInvalid
	}
	return validateDisputeLines(r.Lines, false)
}
func (d Dispute) Validate() error {
	if !validDisputeIDFields(d.Account, d.Scope, d.ID, d.IntentID, d.TransactionID) || !validCurrency(d.Currency) || d.Amount <= 0 || !validDisputeStatus(d.Status) || !billing.ValidID(d.StatusEventID) || d.StatusOccurredAt.IsZero() || d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() || d.Revision < 1 || d.UpdatedAt.Before(d.CreatedAt) || d.StatusOccurredAt.After(d.UpdatedAt) || len(d.Recovered) > maxDisputeLines {
		return billing.ErrInvalid
	}
	if d.DebitAdjustmentID != "" && !billing.ValidID(d.DebitAdjustmentID) {
		return billing.ErrInvalid
	}
	if len(d.Recovered) > 0 && d.DebitAdjustmentID == "" {
		return billing.ErrInvalid
	}
	if _, err := d.StatusOccurredAt.MarshalJSON(); err != nil {
		return billing.ErrInvalid
	}
	if _, err := d.CreatedAt.MarshalJSON(); err != nil {
		return billing.ErrInvalid
	}
	if _, err := d.UpdatedAt.MarshalJSON(); err != nil {
		return billing.ErrInvalid
	}
	if _, err := json.Marshal(d); err != nil {
		return billing.ErrInvalid
	}
	return validateDisputeLines(d.Recovered, true)
}
func (d Dispute) TermsFingerprint() string {
	return digest(struct {
		Account                               billing.AccountID
		Scope                                 billing.Scope
		ID, IntentID, TransactionID, Currency string
		Amount                                int64
	}{d.Account, d.Scope, d.ID, d.IntentID, d.TransactionID, d.Currency, d.Amount})
}
func (d Dispute) Fingerprint() string {
	d.Recovered = sortedDisputeLines(d.Recovered)
	d.StatusOccurredAt = billing.CanonicalTime(d.StatusOccurredAt)
	d.CreatedAt = billing.CanonicalTime(d.CreatedAt)
	d.UpdatedAt = billing.CanonicalTime(d.UpdatedAt)
	return digest(d)
}
func (r DisputeRecord) Validate() error {
	if err := r.Fact.Validate(); err != nil {
		return err
	}
	if r.Result.Account != r.Fact.Account || r.Result.DisputeID != r.Fact.DisputeID || r.Result.EventID != r.Fact.EventID || r.Result.Applied == (r.Result.Rejection != "") || (!r.Result.Applied && (r.Result.StatusApplied || r.Result.RecoveryApplied)) || len(r.Result.Rejection) > 2000 || len(r.Result.IgnoredStatusReason) > 2000 {
		return billing.ErrInvalid
	}
	if r.Result.RecoveryApplied && r.Fact.Recovery == nil {
		return billing.ErrInvalid
	}
	if !r.Result.StatusApplied && r.Result.IgnoredStatusReason == "" {
		return billing.ErrInvalid
	}
	if r.CreatedAt.IsZero() {
		return billing.ErrInvalid
	}
	return nil
}
func (r DisputeRecord) Fingerprint() string {
	r.Fact.OccurredAt = billing.CanonicalTime(r.Fact.OccurredAt)
	if r.Fact.Recovery != nil {
		v := *r.Fact.Recovery
		v.Lines = sortedDisputeLines(v.Lines)
		r.Fact.Recovery = &v
	}
	r.CreatedAt = billing.CanonicalTime(r.CreatedAt)
	return digest(r)
}
func (r DisputeRecoveryRecord) Validate() error {
	if !validDisputeIDFields(r.Account, r.Scope, r.DisputeID, r.IntentID, r.TransactionID) {
		return billing.ErrInvalid
	}
	if err := r.Recovery.Validate(); err != nil {
		return err
	}
	if r.CreatedAt.IsZero() {
		return billing.ErrInvalid
	}
	if _, err := r.CreatedAt.MarshalJSON(); err != nil {
		return billing.ErrInvalid
	}
	if _, err := json.Marshal(r); err != nil {
		return billing.ErrInvalid
	}
	return nil
}
func (r DisputeRecoveryRecord) Fingerprint() string {
	r.Recovery.Lines = sortedDisputeLines(r.Recovery.Lines)
	r.CreatedAt = billing.CanonicalTime(r.CreatedAt)
	return digest(r)
}
func (r DisputeRecoveryRecord) RecoveryFingerprint() string {
	r.Recovery.Lines = sortedDisputeLines(r.Recovery.Lines)
	return digest(struct {
		Account                            billing.AccountID
		Scope                              billing.Scope
		DisputeID, IntentID, TransactionID string
		Recovery                           DisputeRecovery
	}{r.Account, r.Scope, r.DisputeID, r.IntentID, r.TransactionID, r.Recovery})
}
func validateDisputeTime(f DisputeFact, paid time.Time, now time.Time) error {
	if f.OccurredAt.Before(paid) || f.OccurredAt.After(now) {
		return billing.ErrInvalid
	}
	return nil
}
func checkedLineAdd(a, b PaidLine) (PaidLine, error) {
	g, e := checked.Add(a.Gross, b.Gross)
	if e != nil {
		return PaidLine{}, e
	}
	t, e := checked.Add(a.Tax, b.Tax)
	if e != nil {
		return PaidLine{}, e
	}
	return PaidLine{LineID: a.LineID, Gross: g, Tax: t}, nil
}
