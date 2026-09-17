package integration

import (
	"context"
	"errors"

	billing "github.com/data-insights-ai/rho-billing"
)

const MaxWebhookBody = 1 << 20

type WebhookHeaders map[string][]string

func (h WebhookHeaders) clone() WebhookHeaders {
	if h == nil {
		return nil
	}
	out := make(WebhookHeaders, len(h))
	for key, values := range h {
		out[key] = append([]string(nil), values...)
	}
	return out
}

type WebhookVerifier interface {
	Verify(context.Context, []byte, WebhookHeaders) (Message, error)
}

// WebhookReceiver.Receive must return only after the message is committed.
type WebhookReceiver interface {
	Receive(context.Context, Message) error
}

// ReceiveWebhook verifies before persist; invalid verification never reaches the inbox.
func ReceiveWebhook(ctx context.Context, receiver WebhookReceiver, verifier WebhookVerifier, body []byte, headers WebhookHeaders, expected billing.Scope) (Message, error) {
	if receiver == nil || verifier == nil || !expected.Valid() || len(body) > MaxWebhookBody {
		return Message{}, billing.ErrInvalid
	}
	message, err := verifier.Verify(ctx, append([]byte(nil), body...), headers.clone())
	if err != nil {
		return Message{}, err
	}
	if err := message.Validate(); err != nil {
		return Message{}, err
	}
	if message.Direction != Inbound {
		return Message{}, billing.ErrInvalid
	}
	if message.Scope != expected {
		return Message{}, errors.Join(billing.ErrConflict, errors.New("integration: webhook scope mismatch"))
	}
	if err := receiver.Receive(ctx, message); err != nil {
		return Message{}, err
	}
	return message, nil
}
