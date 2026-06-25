package notification

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandlerCreateAndGetNotification(t *testing.T) {
	store := newTestStore(t)
	handler := NewHandlerWithSecurity(store, testSecurityPolicy())
	body := []byte(`{
		"targetUrl":"https://vendor.example.test/callback",
		"method":"POST",
		"headers":{"Content-Type":"application/json"},
		"body":{"event":"registered"},
		"idempotencyKey":"biz-001"
	}`)

	createReq := httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewReader(body))
	createResp := httptest.NewRecorder()
	handler.ServeHTTP(createResp, createReq)

	if createResp.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want %d; body=%s", createResp.Code, http.StatusAccepted, createResp.Body.String())
	}
	var created Notification
	if err := json.Unmarshal(createResp.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Status != StatusPending {
		t.Fatalf("created status = %s, want %s", created.Status, StatusPending)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/notifications/"+created.ID, nil)
	getResp := httptest.NewRecorder()
	handler.ServeHTTP(getResp, getReq)

	if getResp.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d; body=%s", getResp.Code, http.StatusOK, getResp.Body.String())
	}
	var got Notification
	if err := json.Unmarshal(getResp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("got id = %s, want %s", got.ID, created.ID)
	}
}

func TestHandlerRejectsConflictingIdempotencyKey(t *testing.T) {
	store := newTestStore(t)
	handler := NewHandlerWithSecurity(store, testSecurityPolicy())
	first := []byte(`{
		"targetUrl":"https://vendor.example.test/callback",
		"method":"POST",
		"headers":{"Content-Type":"application/json"},
		"body":{"event":"registered","userId":"u_1"},
		"idempotencyKey":"biz-001"
	}`)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewReader(first)))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("first create status = %d, want %d; body=%s", resp.Code, http.StatusAccepted, resp.Body.String())
	}

	conflict := []byte(`{
		"targetUrl":"https://vendor.example.test/callback",
		"method":"POST",
		"headers":{"Content-Type":"application/json"},
		"body":{"event":"registered","userId":"u_2"},
		"idempotencyKey":"biz-001"
	}`)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewReader(conflict)))
	if resp.Code != http.StatusConflict {
		t.Fatalf("conflicting create status = %d, want %d; body=%s", resp.Code, http.StatusConflict, resp.Body.String())
	}
}

func TestHandlerListsAttemptsAndFailedNotifications(t *testing.T) {
	store := newTestStore(t)
	handler := NewHandlerWithSecurity(store, testSecurityPolicy())
	created := createTestNotification(t, store, 1)
	now := created.CreatedAt.Add(time.Second)
	if err := store.RecordAttempt(DeliveryAttempt{
		ID:             "atm_test",
		NotificationID: created.ID,
		AttemptNo:      1,
		Status:         AttemptFailed,
		StatusCode:     500,
		Error:          "vendor returned HTTP 500",
		StartedAt:      now,
		FinishedAt:     now.Add(10 * time.Millisecond),
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	if err := store.MarkFailed(created.ID, "vendor returned HTTP 500", now); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	attemptReq := httptest.NewRequest(http.MethodGet, "/notifications/"+created.ID+"/attempts", nil)
	attemptResp := httptest.NewRecorder()
	handler.ServeHTTP(attemptResp, attemptReq)
	if attemptResp.Code != http.StatusOK {
		t.Fatalf("attempts status = %d, want %d; body=%s", attemptResp.Code, http.StatusOK, attemptResp.Body.String())
	}
	var attempts []DeliveryAttempt
	if err := json.Unmarshal(attemptResp.Body.Bytes(), &attempts); err != nil {
		t.Fatalf("decode attempts response: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt count = %d, want 1", len(attempts))
	}
	if attempts[0].NotificationID != created.ID {
		t.Fatalf("attempt notification id = %s, want %s", attempts[0].NotificationID, created.ID)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/notifications?status=FAILED", nil)
	listResp := httptest.NewRecorder()
	handler.ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d; body=%s", listResp.Code, http.StatusOK, listResp.Body.String())
	}
	var failed []Notification
	if err := json.Unmarshal(listResp.Body.Bytes(), &failed); err != nil {
		t.Fatalf("decode failed response: %v", err)
	}
	if len(failed) != 1 {
		t.Fatalf("failed notification count = %d, want 1", len(failed))
	}
	if failed[0].ID != created.ID {
		t.Fatalf("failed notification id = %s, want %s", failed[0].ID, created.ID)
	}
}

func TestHandlerRejectsDisallowedHostAndHeader(t *testing.T) {
	store := newTestStore(t)
	handler := NewHandlerWithSecurity(store, testSecurityPolicy())

	badHost := []byte(`{
		"targetUrl":"https://evil.example.test/callback",
		"method":"POST",
		"headers":{"Content-Type":"application/json"},
		"body":{"event":"registered"}
	}`)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewReader(badHost)))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("bad host status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}

	badHeader := []byte(`{
		"targetUrl":"https://vendor.example.test/callback",
		"method":"POST",
		"headers":{"Host":"metadata.google.internal"},
		"body":{"event":"registered"}
	}`)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewReader(badHeader)))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("bad header status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
}

func testSecurityPolicy() SecurityPolicy {
	return NewSecurityPolicy([]string{"vendor.example.test"}, DefaultAllowedHeaders())
}
