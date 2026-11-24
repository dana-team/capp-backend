package desiredstate

import (
	"context"
	"time"
)

// Drop removes the whole test database.
func (s *MongoStore) Drop(ctx context.Context) error { return s.objects.Database().Drop(ctx) }

// WithCooldownAt is WithCooldown on a test's clock.
func WithCooldownAt(store Store, cooldown time.Duration, now func() time.Time) Store {
	return &cooldownStore{store: store, cooldown: cooldown, now: now}
}
