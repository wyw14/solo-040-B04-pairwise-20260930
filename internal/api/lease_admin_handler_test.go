package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func postJSON(t *testing.T, handler http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode response %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, decoded
}

func TestReassignLeaseAuthorization(t *testing.T) {
	runtime, err := NewRuntime(context.Background(), "")
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	handler := Router(runtime, ".")
	vessel := uuid.New().String()
	const chamber = "chamber-1"

	// A takes a control lease.
	status, body := postJSON(t, handler, "/api/leases", map[string]any{
		"chamber_id": chamber, "vessel_id": vessel,
		"owner": "A", "ttl_seconds": 60,
	})
	if status != http.StatusOK {
		t.Fatalf("assign status = %d body=%v", status, body)
	}
	tokenA := uint64(body["fencing_token"].(float64))
	if tokenA == 0 {
		t.Fatal("missing fencing token")
	}

	reassignPath := "/api/leases/" + vessel + "/reassign"

	// The original attack: mismatched previous_owner must not move the lease.
	status, body = postJSON(t, handler, reassignPath, map[string]any{
		"chamber_id": chamber, "previous_owner": "not-A",
		"token": tokenA, "new_owner": "B", "ttl_seconds": 60,
	})
	if status != http.StatusForbidden {
		t.Fatalf("forged takeover status = %d, want 403, body=%v", status, body)
	}

	// Missing token / owner proof is a bad request.
	status, _ = postJSON(t, handler, reassignPath, map[string]any{
		"chamber_id": chamber, "previous_owner": "A",
		"new_owner": "B", "ttl_seconds": 60,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("proofless takeover status = %d, want 400", status)
	}

	// Stale token is rejected too.
	status, _ = postJSON(t, handler, reassignPath, map[string]any{
		"chamber_id": chamber, "previous_owner": "A",
		"token": tokenA + 100, "new_owner": "B", "ttl_seconds": 60,
	})
	if status != http.StatusForbidden {
		t.Fatalf("stale-token takeover status = %d, want 403", status)
	}

	// A's heartbeat still works — control was never lost.
	status, body = postJSON(t, handler, "/api/leases/"+vessel+"/heartbeat", map[string]any{
		"chamber_id": chamber, "owner": "A",
		"token": tokenA, "ttl_seconds": 60,
	})
	if status != http.StatusOK {
		t.Fatalf("A heartbeat after failed takeovers = %d body=%v", status, body)
	}

	// Explicitly authorized succession: A's owner identity + current token.
	status, body = postJSON(t, handler, reassignPath, map[string]any{
		"chamber_id": chamber, "previous_owner": "A",
		"token": tokenA, "new_owner": "B", "ttl_seconds": 60,
	})
	if status != http.StatusOK {
		t.Fatalf("authorized takeover status = %d body=%v", status, body)
	}
	tokenB := uint64(body["fencing_token"].(float64))
	if body["owner"] != "B" || tokenB <= tokenA {
		t.Fatalf("unexpected new lease: %v", body)
	}

	// Old fence token is dead: A's heartbeat is rejected as stale.
	status, _ = postJSON(t, handler, "/api/leases/"+vessel+"/heartbeat", map[string]any{
		"chamber_id": chamber, "owner": "A",
		"token": tokenA, "ttl_seconds": 60,
	})
	if status != http.StatusConflict {
		t.Fatalf("A post-takeover heartbeat status = %d, want 409", status)
	}

	// B's new token is the one that renews.
	status, body = postJSON(t, handler, "/api/leases/"+vessel+"/heartbeat", map[string]any{
		"chamber_id": chamber, "owner": "B",
		"token": tokenB, "ttl_seconds": 60,
	})
	if status != http.StatusOK {
		t.Fatalf("B heartbeat status = %d body=%v", status, body)
	}
	if uint64(body["fencing_token"].(float64)) != tokenB {
		t.Fatalf("B heartbeat returned token %v, want %d", body["fencing_token"], tokenB)
	}

	// A cannot assign over B's active lease.
	status, _ = postJSON(t, handler, "/api/leases", map[string]any{
		"chamber_id": chamber, "vessel_id": vessel,
		"owner": "A", "ttl_seconds": 60,
	})
	if status != http.StatusConflict {
		t.Fatalf("assign over active lease status = %d, want 409", status)
	}
}
