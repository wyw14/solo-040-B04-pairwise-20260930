package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/wyw14/cry-96/internal/lease"
)

func expiredLeasesHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, runtime.LeaseSweeper.Expired())
	}
}

type reassignInput struct {
	ChamberID     string `json:"chamber_id"`
	PreviousOwner string `json:"previous_owner"`
	Token         uint64 `json:"token"`
	NewOwner      string `json:"new_owner"`
	TTLSeconds    int    `json:"ttl_seconds"`
}

// reassignLeaseHandler hands a live lease to a successor. The request must
// carry the chamber, the exact current owner, and that owner's current fencing
// token — the explicit authorization path enforced atomically by the lease
// service. The response carries the successor's brand-new fencing token; the
// old token is dead the moment the transfer is journaled.
func reassignLeaseHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input reassignInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if input.ChamberID == "" || input.PreviousOwner == "" || input.NewOwner == "" || input.Token == 0 {
			writeError(w, http.StatusBadRequest, errors.New("chamber_id, previous_owner, token and new_owner are required"))
			return
		}
		vesselID, err := parseUUID(chi.URLParam(r, "vesselID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		result, err := runtime.Leases.Takeover(
			r.Context(), input.ChamberID, vesselID,
			input.PreviousOwner, input.Token, input.NewOwner,
			durationSeconds(input.TTLSeconds),
		)
		if err != nil {
			writeError(w, leaseErrorStatus(err), err)
			return
		}
		// Defensive consistency check: the journaled lease, the returned
		// token and the admission gate must agree before we respond.
		if !runtime.Leases.Authorized(vesselID, input.NewOwner, result.FencingToken) {
			writeError(w, http.StatusConflict, errors.New("transferred lease is not authorized for admission"))
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func leaseErrorStatus(err error) int {
	switch {
	case errors.Is(err, lease.ErrNotAuthorized):
		return http.StatusForbidden
	case errors.Is(err, lease.ErrLeaseStillActive),
		errors.Is(err, lease.ErrLeaseExpired),
		errors.Is(err, lease.ErrNoLease),
		errors.Is(err, lease.ErrIdenticalOwners):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

type releaseLeaseInput struct {
	ChamberID string `json:"chamber_id"`
	Owner     string `json:"owner"`
	Token     uint64 `json:"token"`
}

func releaseLeaseHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input releaseLeaseInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		vesselID, err := parseUUID(chi.URLParam(r, "vesselID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := runtime.Leases.Release(r.Context(), input.ChamberID, vesselID, input.Owner, input.Token); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"released": true})
	}
}
