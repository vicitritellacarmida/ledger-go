package service

import (
	"context"

	"github.com/vicitritellacarmida/ledger-go/internal/domain"
	"github.com/vicitritellacarmida/ledger-go/internal/store"
)

// ClosePeriod closes an accounting month. After closing, no entry may have
// an effective date inside it; late corrections must be dated in an open
// period. Closing cannot be undone through the API.
func (s *Service) ClosePeriod(ctx context.Context, period string) (*store.ClosedPeriod, error) {
	p, err := domain.ParsePeriod(period)
	if err != nil {
		return nil, err
	}
	if err := p.CanClose(s.Today()); err != nil {
		return nil, err
	}
	out := &store.ClosedPeriod{Period: p.String()}
	err = s.store.InTx(ctx, func(ctx context.Context) error {
		// Taking the chain head lock waits for in-flight postings to finish
		// and blocks new ones until the period row is committed.
		if _, _, err := s.store.LockChainHead(ctx); err != nil {
			return err
		}
		out.ClosedAt = s.now()
		return s.store.InsertClosedPeriod(ctx, out.Period, out.ClosedAt)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListClosedPeriods returns closed periods, latest first.
func (s *Service) ListClosedPeriods(ctx context.Context) ([]store.ClosedPeriod, error) {
	return s.store.ListClosedPeriods(ctx)
}
