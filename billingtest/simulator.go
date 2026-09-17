// Package billingtest contains deterministic provider simulators for domain
// tests. It does not make network calls or model provider SDK behavior.
package billingtest

import (
	"context"
	"errors"
	"fmt"
	"sync"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

var ErrAcceptedThenTimeout = errors.New("billingtest: accepted provider call timed out")

type Behavior string

const (
	Accept            Behavior = "accept"
	Reject            Behavior = "reject"
	AcceptThenTimeout Behavior = "accept_then_timeout"
	Pending           Behavior = "pending"
)

type SubmitRequest struct {
	BatchID        string
	AttemptID      string
	IdempotencyKey string
	Currency       string
	Amount         int64
}

type Simulator struct {
	mu        sync.Mutex
	behavior  Behavior
	next      int
	requests  map[string]SubmitRequest
	responses map[string]usage.ProviderResult
	timeouts  map[string]bool
	submits   int
	lookups   int
}

func NewSimulator(behavior ...Behavior) *Simulator {
	chosen := Accept
	if len(behavior) > 0 {
		chosen = behavior[0]
	}
	return &Simulator{behavior: chosen, requests: map[string]SubmitRequest{}, responses: map[string]usage.ProviderResult{}, timeouts: map[string]bool{}}
}

func (s *Simulator) SetBehavior(behavior Behavior) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.behavior = behavior
}

func (s *Simulator) Submit(ctx context.Context, request SubmitRequest) (usage.ProviderResult, error) {
	if err := ctx.Err(); err != nil {
		return usage.ProviderResult{}, err
	}
	if request.IdempotencyKey == "" || request.BatchID == "" || request.AttemptID == "" || request.Currency == "" || request.Amount < 0 {
		return usage.ProviderResult{}, billing.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submits++
	if old, exists := s.requests[request.IdempotencyKey]; exists {
		if old != request {
			return usage.ProviderResult{}, billing.ErrConflict
		}
		response := s.responses[request.IdempotencyKey]
		if s.timeouts[request.IdempotencyKey] {
			delete(s.timeouts, request.IdempotencyKey)
		}
		return response, nil
	}
	s.requests[request.IdempotencyKey] = request
	s.next++
	response := usage.ProviderResult{ProviderReference: fmt.Sprintf("sim-charge-%d", s.next)}
	s.responses[request.IdempotencyKey] = response
	switch s.behavior {
	case Accept:
		response.Status = usage.SubmissionConfirmed
	case Reject:
		response.Status = usage.SubmissionRejected
		response.Reason = "simulated rejection"
	case AcceptThenTimeout:
		response.Status = usage.SubmissionConfirmed
		s.timeouts[request.IdempotencyKey] = true
		s.responses[request.IdempotencyKey] = response
		return usage.ProviderResult{Status: usage.SubmissionUnknown, Reason: "response lost"}, ErrAcceptedThenTimeout
	case Pending:
		response.Status = usage.SubmissionUnknown
		response.ProviderReference = ""
		response.Reason = "provider still processing"
	default:
		return usage.ProviderResult{}, billing.ErrInvalid
	}
	s.responses[request.IdempotencyKey] = response
	return response, nil
}

// Lookup is authoritative for the submitted idempotency key. A missing key is
// an explicit provider-not-found result, allowing tests to resolve an unknown
// local attempt to rejected without a blind resubmission.
func (s *Simulator) Lookup(ctx context.Context, idempotencyKey string) (usage.ProviderResult, error) {
	if err := ctx.Err(); err != nil {
		return usage.ProviderResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups++
	response, ok := s.responses[idempotencyKey]
	if !ok {
		return usage.ProviderResult{Status: usage.SubmissionRejected, Reason: "provider record not found"}, nil
	}
	delete(s.timeouts, idempotencyKey)
	return response, nil
}

func (s *Simulator) SubmitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submits
}

func (s *Simulator) LookupCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookups
}

// Resolve advances a delayed provider operation. Tests control when evidence
// becomes authoritative instead of depending on wall-clock sleeps.
func (s *Simulator) Resolve(key string, result usage.ProviderResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.responses[key]
	if !ok {
		return billing.ErrNotFound
	}
	if result.Status != usage.SubmissionConfirmed && result.Status != usage.SubmissionRejected {
		return billing.ErrInvalid
	}
	if result.Status == usage.SubmissionConfirmed && result.ProviderReference == "" || result.Status == usage.SubmissionRejected && result.Reason == "" {
		return billing.ErrInvalid
	}
	if old.Status != usage.SubmissionUnknown {
		if old == result {
			return nil
		}
		return billing.ErrConflict
	}
	s.responses[key] = result
	return nil
}
