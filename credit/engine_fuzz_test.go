package credit

import (
	"context"
	"testing"
	"time"
)

func FuzzCreditConservation(f *testing.F) {
	f.Add(int64(10), int64(3))
	f.Add(int64(1), int64(0))
	f.Fuzz(func(t *testing.T, amountInput, consumedInput int64) {
		amount := boundedPositive(amountInput, 1000)
		consumed := bounded(consumedInput, amount)
		engine, _, clock := newTestEngine()
		grant(t, engine, clock, "fuzz-lot", amount, 0, "")
		reserve(t, engine, clock, "fuzz-reserve", "fuzz-res", amount, time.Hour, "actor", "")
		if _, err := engine.Settle(context.Background(), SettleInput{
			Account:       testAccount,
			Operation:     "fuzz-settle",
			ReservationID: "fuzz-res",
			Actual:        consumed,
			Evidence: Evidence{
				UsageID:       "fuzz-usage",
				RatingVersion: "rating-v1",
				Metrics:       []Metric{{Name: "units", Quantity: consumed}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		got := balance(t, engine, testUnit, "")
		if got.Available+got.Held+got.Consumed+got.Expired+got.Revoked != amount {
			t.Fatalf("conservation failed: amount=%d consumed=%d balance=%+v", amount, consumed, got)
		}
		if got.Consumed != consumed || got.Held != 0 {
			t.Fatalf("settlement projection: amount=%d consumed=%d balance=%+v", amount, consumed, got)
		}
	})
}

func boundedPositive(value, maximum int64) int64 {
	if maximum < 1 {
		return 1
	}
	if value == -1<<63 {
		return 1
	}
	value %= maximum
	if value < 0 {
		value = -value
	}
	return value + 1
}

func bounded(value, maximum int64) int64 {
	if maximum <= 0 {
		return 0
	}
	if value == -1<<63 {
		return 0
	}
	value %= maximum + 1
	if value < 0 {
		value = -value
	}
	return value
}
