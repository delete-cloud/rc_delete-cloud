package notification

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type Handler struct {
	store  Store
	policy SecurityPolicy
	mux    *http.ServeMux
}

func NewHandler(store Store) http.Handler {
	return NewHandlerWithSecurity(store, DefaultSecurityPolicy())
}

func NewHandlerWithSecurity(store Store, policy SecurityPolicy) http.Handler {
	if store == nil {
		panic("store is required")
	}
	h := &Handler{store: store, policy: policy, mux: http.NewServeMux()}
	h.routes()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) routes() {
	h.mux.HandleFunc("GET /healthz", h.health)
	h.mux.HandleFunc("POST /notifications", h.create)
	h.mux.HandleFunc("GET /notifications", h.list)
	h.mux.HandleFunc("GET /notifications/{id}", h.get)
	h.mux.HandleFunc("GET /notifications/{id}/attempts", h.attempts)
	h.mux.HandleFunc("POST /notifications/{id}/retry", h.retry)
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req CreateRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request: "+err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.policy.ValidateEnvelope(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, duplicate, err := h.store.Create(NewNotification(req, time.Now()))
	if errors.Is(err, ErrIdempotencyConflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusAccepted
	if duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, created)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	statusValue := strings.TrimSpace(r.URL.Query().Get("status"))
	if statusValue == "" {
		writeError(w, http.StatusBadRequest, "status query is required")
		return
	}
	status, err := ParseStatus(statusValue)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	items, err := h.store.ListByStatus(status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	n, err := h.store.Get(id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "notification not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (h *Handler) attempts(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	attempts, err := h.store.ListAttempts(id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "notification not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, attempts)
}

func (h *Handler) retry(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	n, err := h.store.RetryFailed(id, time.Now())
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "notification not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, n)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
