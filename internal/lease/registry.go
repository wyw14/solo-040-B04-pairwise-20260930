package lease

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

// Sentinel errors let callers distinguish rejection reasons without leaking
// the identity of the current owner.
var (
	ErrLeaseStillActive = errors.New("lease is still active")
	ErrNotAuthorized    = errors.New("lease owner or fencing token is stale")
	ErrLeaseExpired     = errors.New("lease has expired")
	ErrNoLease          = errors.New("no lease for vessel")
	ErrIdenticalOwners  = errors.New("new owner must differ from the current owner")
)

// Registry owns monotonically increasing fencing tokens per vessel.
// Every transition that can replace an owner performs its ownership and expiry
// checks in the same critical section as the write, so a renewal and a
// takeover can never interleave against a stale read.
type Registry struct {
	mu        sync.RWMutex
	leases    map[uuid.UUID]model.Lease
	nextToken map[uuid.UUID]uint64
}

func NewRegistry() *Registry {
	return &Registry{
		leases:    make(map[uuid.UUID]model.Lease),
		nextToken: make(map[uuid.UUID]uint64),
	}
}

// Assign grants a fresh lease only when the vessel currently has none, or its
// existing lease has expired. A live lease can move only through Takeover,
// with proof from the current owner.
func (r *Registry) Assign(vesselID uuid.UUID, owner string, ttl time.Duration, now time.Time) (model.Lease, error) {
	if vesselID == uuid.Nil || owner == "" || ttl <= 0 {
		return model.Lease{}, errors.New("assignment requires vessel, owner and positive ttl")
	}
	now = now.UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.leases[vesselID]; ok && current.Active(now) {
		return model.Lease{}, ErrLeaseStillActive
	}
	return r.grantLocked(vesselID, owner, ttl, now), nil
}

// Takeover transfers an ACTIVE lease from previousOwner to newOwner. It is the
// only way to displace a live lease: the caller must prove the current owner
// and the current fencing token, and the lease must be unexpired at the
// instant of the write. A fresh fencing token is issued, so the previous
// owner's token can never authorize anything afterwards.
func (r *Registry) Takeover(vesselID uuid.UUID, previousOwner string, previousToken uint64, newOwner string, ttl time.Duration, now time.Time) (model.Lease, error) {
	if vesselID == uuid.Nil || previousOwner == "" || newOwner == "" || ttl <= 0 {
		return model.Lease{}, errors.New("takeover requires vessel, both owners and positive ttl")
	}
	if previousToken == 0 {
		return model.Lease{}, ErrNotAuthorized
	}
	if previousOwner == newOwner {
		return model.Lease{}, ErrIdenticalOwners
	}
	now = now.UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.leases[vesselID]
	if !ok {
		return model.Lease{}, ErrNoLease
	}
	if !current.Active(now) {
		return model.Lease{}, ErrLeaseExpired
	}
	if current.Owner != previousOwner || current.FencingToken != previousToken {
		return model.Lease{}, ErrNotAuthorized
	}
	return r.grantLocked(vesselID, newOwner, ttl, now), nil
}

// grantLocked issues the next monotonically increasing fencing token.
// Callers must hold mu.
func (r *Registry) grantLocked(vesselID uuid.UUID, owner string, ttl time.Duration, now time.Time) model.Lease {
	r.nextToken[vesselID]++
	lease := model.Lease{
		VesselID: vesselID, Owner: owner, FencingToken: r.nextToken[vesselID],
		ExpiresAt: now.Add(ttl),
	}
	r.leases[vesselID] = lease
	return lease
}

func (r *Registry) Renew(vesselID uuid.UUID, owner string, token uint64, ttl time.Duration, now time.Time) (model.Lease, error) {
	if ttl <= 0 {
		return model.Lease{}, errors.New("renewal ttl must be positive")
	}
	if owner == "" || token == 0 {
		return model.Lease{}, ErrNotAuthorized
	}
	now = now.UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	lease, ok := r.leases[vesselID]
	if !ok {
		return model.Lease{}, ErrNoLease
	}
	if !lease.Active(now) {
		return model.Lease{}, ErrLeaseExpired
	}
	if lease.Owner != owner || lease.FencingToken != token {
		return model.Lease{}, ErrNotAuthorized
	}
	lease.ExpiresAt = now.Add(ttl)
	r.leases[vesselID] = lease
	return lease, nil
}

func (r *Registry) Get(vesselID uuid.UUID) (model.Lease, bool) {
	r.mu.RLock()
	lease, ok := r.leases[vesselID]
	r.mu.RUnlock()
	return lease, ok
}

func (r *Registry) Release(vesselID uuid.UUID, owner string, token uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	lease, ok := r.leases[vesselID]
	if !ok || lease.Owner != owner || lease.FencingToken != token {
		return false
	}
	// Keep the fencing sequence in nextToken; only the live entry is removed.
	delete(r.leases, vesselID)
	return true
}

func (r *Registry) Snapshot() []model.Lease {
	r.mu.RLock()
	result := make([]model.Lease, 0, len(r.leases))
	for _, lease := range r.leases {
		result = append(result, lease)
	}
	r.mu.RUnlock()
	return result
}
