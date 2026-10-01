package vpngate

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// API is the lease HTTP API (v1) that Sub2API's proxy pool mode talks to.
//
//	POST   /v1/leases              {"client_ref": "...", "protocol": "http"|"socks5"}
//	POST   /v1/leases/{id}/rotate  move the lease to another random node
//	DELETE /v1/leases/{id}         release
//	GET    /v1/leases              list (without passwords)
//	GET    /healthz                counts
//
// Every request needs "Authorization: Bearer <token>". A lease's endpoint
// (host, port, username, password) never changes for the lease's lifetime;
// rotating only changes the VPN Gate node behind it.
type API struct {
	Token      string
	PublicHost string
	Master     string
	Store      *LeaseStore
	Mgr        *Manager
	NodeCount  int
	// Pool, when set, reports the pool the running config was built from;
	// /healthz then reports its candidates as "nodes".
	Pool func() PoolStatus
	Logf func(format string, args ...any)

	mu sync.Mutex // serialises lease, rotate and release
}

// LeaseView is the JSON form of a lease.
type LeaseView struct {
	LeaseID   string    `json:"lease_id"`
	ClientRef string    `json:"client_ref"`
	Protocol  string    `json:"protocol"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Username  string    `json:"username"`
	Password  string    `json:"password,omitempty"`
	Node      string    `json:"node"`
	CreatedAt time.Time `json:"created_at"`
}

// PoolStatus describes the node pool the running Mihomo config was built from.
type PoolStatus struct {
	LoadedAt   time.Time
	SHA256     string
	Candidates int
	Retained   int
}

// shortSum shortens a sha256 hex digest for logs and /healthz.
func shortSum(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// Pause blocks lease, rotate and release until resume is called.
func (a *API) Pause() (resume func()) {
	a.mu.Lock()
	return sync.OnceFunc(a.mu.Unlock)
}

const apiTimeout = 2 * time.Minute

// Handler returns the API's HTTP handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/leases", a.createLease)
	mux.HandleFunc("POST /v1/leases/{id}/rotate", a.rotateLease)
	mux.HandleFunc("DELETE /v1/leases/{id}", a.deleteLease)
	mux.HandleFunc("GET /v1/leases", a.listLeases)
	mux.HandleFunc("GET /healthz", a.healthz)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !hasBearer || a.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), apiTimeout)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

func (a *API) view(ctx context.Context, l Lease, withPassword bool) LeaseView {
	v := LeaseView{
		LeaseID:   l.ID,
		ClientRef: l.ClientRef,
		Protocol:  l.Protocol,
		Host:      a.PublicHost,
		Username:  l.Slot,
		CreatedAt: l.CreatedAt,
	}
	if s, ok := a.Store.SlotByName(l.Slot); ok {
		v.Port = s.Port
		if node, err := a.Mgr.CurrentNode(ctx, s); err == nil {
			v.Node = node
		}
	}
	if withPassword {
		v.Password = SlotPassword(a.Master, l.Slot)
	}
	return v
}

func (a *API) createLease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientRef string `json:"client_ref"`
		Protocol  string `json:"protocol"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	req.ClientRef = strings.TrimSpace(req.ClientRef)
	if req.ClientRef == "" || len(req.ClientRef) > 100 {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	switch req.Protocol {
	case "":
		req.Protocol = "http"
	case "http", "socks5":
	default:
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if l, ok := a.Store.ByClientRef(req.ClientRef); ok {
		writeJSON(w, http.StatusOK, a.view(r.Context(), l, true))
		return
	}
	slot, err := a.Store.FreeSlot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "pool_exhausted")
		return
	}
	// A fresh random node, never the one the slot's previous lease used.
	if _, err := a.Mgr.Reselect(r.Context(), slot); err != nil {
		a.logf("vpngate: lease for %q on %s: %v", req.ClientRef, slot.Name, err)
		writeError(w, http.StatusServiceUnavailable, "no_healthy_node")
		return
	}
	l, err := a.Store.Add(req.ClientRef, req.Protocol, slot)
	if err != nil {
		a.logf("vpngate: save lease: %v", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	a.logf("vpngate: leased %s to %q as %s", slot.Name, req.ClientRef, l.ID)
	writeJSON(w, http.StatusOK, a.view(r.Context(), l, true))
}

func (a *API) rotateLease(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.Store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	slot, ok := a.Store.SlotByName(l.Slot)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if _, err := a.Mgr.Reselect(r.Context(), slot); err != nil {
		a.logf("vpngate: rotate %s (%s): %v", l.ID, slot.Name, err)
		if errors.Is(err, ErrNoHealthyNode) {
			writeError(w, http.StatusServiceUnavailable, "no_healthy_node")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeJSON(w, http.StatusOK, a.view(r.Context(), l, true))
}

func (a *API) deleteLease(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := r.PathValue("id")
	existed, err := a.Store.Remove(id)
	if err != nil {
		a.logf("vpngate: save leases: %v", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	a.logf("vpngate: released %s", id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) listLeases(w http.ResponseWriter, r *http.Request) {
	leases := a.Store.List()
	out := make([]LeaseView, 0, len(leases))
	for _, l := range leases {
		out = append(out, a.view(r.Context(), l, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"leases": out})
}

func (a *API) healthz(w http.ResponseWriter, _ *http.Request) {
	body := map[string]any{
		"status": "ok",
		"nodes":  a.NodeCount,
		"slots":  len(a.Store.slots),
		"leased": len(a.Store.List()),
	}
	if a.Pool != nil {
		st := a.Pool()
		body["nodes"] = st.Candidates
		body["candidates"] = st.Candidates
		body["retained"] = st.Retained
		body["pool_sha256"] = shortSum(st.SHA256)
		body["pool_loaded_at"] = st.LoadedAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
