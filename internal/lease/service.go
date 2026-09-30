package lease

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// Service journals ownership changes and exposes admission authorization.
type Service struct {
	registry  *Registry
	admission *admission.Controller
	recorder  *journal.Recorder
	now       func() time.Time
}

func NewService(registry *Registry, admissionController *admission.Controller, recorder *journal.Recorder, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{registry: registry, admission: admissionController, recorder: recorder, now: now}
}

func (s *Service) Assign(ctx context.Context, chamberID string, vesselID uuid.UUID, owner string, ttl time.Duration) (model.Lease, error) {
	lease, err := s.registry.Assign(vesselID, owner, ttl, s.now())
	if err != nil {
		return model.Lease{}, err
	}
	if err := s.recordLease(ctx, chamberID, 0, lease); err != nil {
		return model.Lease{}, err
	}
	return lease, nil
}

func (s *Service) Heartbeat(ctx context.Context, chamberID string, vesselID uuid.UUID, owner string, token uint64, ttl time.Duration) (model.Lease, error) {
	lease, err := s.registry.Renew(vesselID, owner, token, ttl, s.now())
	if err != nil {
		return model.Lease{}, err
	}
	if err := s.recordLease(ctx, chamberID, token, lease); err != nil {
		return model.Lease{}, err
	}
	return lease, nil
}

// Takeover hands a live lease to newOwner. The previous owner must present the
// chamber, the exact current owner identity and its current fencing token; the
// registry verifies all three atomically against an unexpired lease. The new
// lease carries a new fencing token, which is the value journaled and the only
// value admission will accept afterwards.
func (s *Service) Takeover(ctx context.Context, chamberID string, vesselID uuid.UUID, previousOwner string, previousToken uint64, newOwner string, ttl time.Duration) (model.Lease, error) {
	if chamberID == "" {
		return model.Lease{}, errors.New("chamber_id is required")
	}
	if newOwner == "" {
		return model.Lease{}, errors.New("new_owner is required")
	}
	lease, err := s.registry.Takeover(vesselID, previousOwner, previousToken, newOwner, ttl, s.now())
	if err != nil {
		return model.Lease{}, err
	}
	if err := s.recordLease(ctx, chamberID, lease.FencingToken, lease); err != nil {
		return model.Lease{}, err
	}
	return lease, nil
}

// recordLease journals the lease exactly as persisted, so the new fencing
// token, the event log and admission authorization can never disagree.
func (s *Service) recordLease(ctx context.Context, chamberID string, token uint64, lease model.Lease) error {
	_, err := s.recorder.Record(ctx, chamberID, model.EventLeaseChanged, uuid.Nil, token, lease)
	return err
}

func (s *Service) Authorized(vesselID uuid.UUID, owner string, token uint64) bool {
	lease, ok := s.registry.Get(vesselID)
	return ok && s.admission.OwnerAllowed(vesselID, owner, token, lease, s.now())
}

func (s *Service) Release(ctx context.Context, chamberID string, vesselID uuid.UUID, owner string, token uint64) error {
	if !s.registry.Release(vesselID, owner, token) {
		return errors.New("lease owner or fencing token is stale")
	}
	_, err := s.recorder.Record(ctx, chamberID, model.EventLeaseChanged, uuid.Nil, token, map[string]any{
		"vessel_id": vesselID, "owner": owner, "released": true,
	})
	return err
}
