package wager

import (
	"context"

	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
)

// GetTransaction loads a transaction by internal identity.
func (s *Service) GetTransaction(ctx context.Context, id uuid.UUID) (*wagering.Transaction, error) {
	return s.transactions.GetByID(ctx, id)
}

// GetByProviderExternal loads a transaction by provider and external identity.
func (s *Service) GetByProviderExternal(ctx context.Context, providerID, externalID string) (*wagering.Transaction, error) {
	return s.transactions.GetByProviderExternal(ctx, providerID, externalID)
}

// CountPending returns how many transactions wait for processing or a reference.
func (s *Service) CountPending(ctx context.Context) (int64, error) {
	return s.transactions.CountPending(ctx)
}
