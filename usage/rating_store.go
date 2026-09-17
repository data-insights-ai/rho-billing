package usage

import "context"

type RatingStore interface {
	PublishRating(context.Context, RuleConfig) error
	Rating(context.Context, string) (*Rule, error)
}
