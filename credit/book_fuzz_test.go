package credit_test

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/credit"
)

// Whatever happens, every lot's counters add up, nothing goes negative and
// spending never exceeds what was granted.
func FuzzCreditIsConserved(f *testing.F) {
	f.Add([]byte{1, 40, 2, 10, 3, 60, 4, 0, 5, 9})
	f.Fuzz(func(t *testing.T, ops []byte) {
		lots := []credit.Lot{}
		for i, days := range []int{0, 3, 9} {
			l, _ := credit.NewLot(string(rune('a'+i)), "", int64(50+i*25), t0, t0, expiry(days))
			lots = append(lots, l)
		}
		b, err := credit.Open(lots)
		if err != nil {
			t.Fatal(err)
		}
		var open []credit.Reservation
		at := t0
		var takenBack, spentAfterRefund int64
		for i := 0; i+1 < len(ops); i += 2 {
			amount := int64(ops[i+1])
			switch ops[i] % 7 {
			case 0:
				at = at.Add(time.Duration(amount) * time.Hour)
				b.Expire(at)
			case 1:
				if r, err := b.Reserve("r"+strconv.Itoa(i), "", amount, at); err == nil {
					open = append(open, r)
				}
			case 2:
				if len(open) > 0 {
					if st, err := b.Settle(open[0], amount, at); err == nil {
						spentAfterRefund += st.Refunded
					}
					open = open[1:]
				}
			case 3:
				if len(open) > 0 {
					_ = b.Cancel(open[0], at)
					open = open[1:]
				}
			case 4:
				_, _ = b.Spend("", amount, at, "")
			case 5:
				id := lots[int(amount)%len(lots)].ID
				before, _ := b.Lot(id)
				if b.Revoke(id, at) == nil && before.RevokedAt.IsZero() {
					takenBack += before.Available + before.Held - before.PendingRevoke
				}
			case 6:
				if short, err := b.Withdraw(lots[int(amount)%len(lots)].ID, amount/2+1, at); err == nil {
					takenBack += amount/2 + 1 - short
				}
			}
		}
		for _, r := range open {
			if err := b.Cancel(r, at); err != nil && !errors.Is(err, credit.ErrEnded) && !errors.Is(err, credit.ErrUnknownLot) {
				t.Fatal(err)
			}
		}
		var revoked, pending int64
		for _, l := range lots {
			now, _ := b.Lot(l.ID)
			revoked += now.Revoked
			pending += now.PendingRevoke
		}
		if pending != 0 {
			t.Fatalf("%d refunded credit still pending after every reservation ended", pending)
		}
		if takenBack != revoked+pending+spentAfterRefund {
			t.Fatalf("refunds took back %d; revoked %d, pending %d, spent after the refund %d", takenBack, revoked, pending, spentAfterRefund)
		}
		for _, l := range lots {
			now, _ := b.Lot(l.ID)
			if now.Available < 0 || now.Held < 0 || now.Consumed < 0 || now.Expired < 0 || now.Revoked < 0 ||
				now.PendingRevoke < 0 || now.PendingRevoke > now.Held ||
				now.Available+now.Held+now.Consumed+now.Expired+now.Revoked != now.Initial {
				t.Fatalf("lot %s broken: %+v", l.ID, now)
			}
		}
	})
}

func expiry(days int) time.Time {
	if days == 0 {
		return time.Time{}
	}
	return t0.AddDate(0, 0, days)
}
