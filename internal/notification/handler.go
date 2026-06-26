package notification

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Handler struct {
	store  Store
	policy SecurityPolicy
	mux    *http.ServeMux
}

type notificationListResponse struct {
	Items      []Notification `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

type listCursor struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
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
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON request: trailing JSON token")
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
	options, err := parseListOptions(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.store.ListByStatus(status, options)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	response := notificationListResponse{Items: result.Items}
	if result.HasMore && len(result.Items) > 0 {
		nextCursor, err := encodeListCursor(result.Items[len(result.Items)-1])
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		response.NextCursor = nextCursor
	}
	writeJSON(w, http.StatusOK, response)
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

func parseListOptions(r *http.Request) (ListOptions, error) {
	query := r.URL.Query()
	limit := DefaultListLimit
	limitValue := strings.TrimSpace(query.Get("limit"))
	if limitValue != "" {
		parsed, err := strconv.Atoi(limitValue)
		if err != nil || parsed <= 0 {
			return ListOptions{}, errors.New("limit must be a positive integer")
		}
		limit = normalizeListLimit(parsed)
	}
	options := ListOptions{Limit: limit}
	cursorValue := strings.TrimSpace(query.Get("cursor"))
	if cursorValue == "" {
		return options, nil
	}
	cursor, err := decodeListCursor(cursorValue)
	if err != nil {
		return ListOptions{}, err
	}
	options.AfterCreatedAt = &cursor.CreatedAt
	options.AfterID = cursor.ID
	return options, nil
}

func encodeListCursor(n Notification) (string, error) {
	payload, err := json.Marshal(listCursor{CreatedAt: n.CreatedAt, ID: n.ID})
	if err != nil {
		return "", fmt.Errorf("encode list cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeListCursor(value string) (listCursor, error) {
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return listCursor{}, errors.New("cursor is invalid")
	}
	var cursor listCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return listCursor{}, errors.New("cursor is invalid")
	}
	if cursor.CreatedAt.IsZero() || strings.TrimSpace(cursor.ID) == "" {
		return listCursor{}, errors.New("cursor is invalid")
	}
	return cursor, nil
}
