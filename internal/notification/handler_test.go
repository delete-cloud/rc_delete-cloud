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

func TestHandlerRejectsTrailingJSONToken(t *testing.T) {
	store := newTestStore(t)
	handler := NewHandlerWithSecurity(store, testSecurityPolicy())
	body := []byte(`{
		"targetUrl":"https://vendor.example.test/callback",
		"method":"POST",
		"headers":{"Content-Type":"application/json"},
		"body":{"event":"registered"}
	}{"targetUrl":"https://vendor.example.test/other"}`)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewReader(body)))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("create with trailing JSON status = %d, want %d; body=%s", resp.Code, http.StatusBadRequest, resp.Body.String())
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
	claimed, err := store.ClaimDue(now, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim notification: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed count = %d, want 1", len(claimed))
	}
	if err := store.MarkFailed(created.ID, "vendor returned HTTP 500", claimed[0].AttemptCount, now); err != nil {
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
	var failed notificationListResponse
	if err := json.Unmarshal(listResp.Body.Bytes(), &failed); err != nil {
		t.Fatalf("decode failed response: %v", err)
	}
	if len(failed.Items) != 1 {
		t.Fatalf("failed notification count = %d, want 1", len(failed.Items))
	}
	if failed.Items[0].ID != created.ID {
		t.Fatalf("failed notification id = %s, want %s", failed.Items[0].ID, created.ID)
	}
}

func TestHandlerListsNotificationsWithLimitAndCursor(t *testing.T) {
	store := newTestStore(t)
	handler := NewHandlerWithSecurity(store, testSecurityPolicy())
	base := time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	first := createFailedNotificationAt(t, store, base, "first")
	second := createFailedNotificationAt(t, store, base.Add(time.Second), "second")
	third := createFailedNotificationAt(t, store, base.Add(2*time.Second), "third")

	firstReq := httptest.NewRequest(http.MethodGet, "/notifications?status=FAILED&limit=2", nil)
	firstResp := httptest.NewRecorder()
	handler.ServeHTTP(firstResp, firstReq)
	if firstResp.Code != http.StatusOK {
		t.Fatalf("first list status = %d, want %d; body=%s", firstResp.Code, http.StatusOK, firstResp.Body.String())
	}
	var firstPage notificationListResponse
	if err := json.Unmarshal(firstResp.Body.Bytes(), &firstPage); err != nil {
		t.Fatalf("decode first list response: %v", err)
	}
	if len(firstPage.Items) != 2 {
		t.Fatalf("first page count = %d, want 2", len(firstPage.Items))
	}
	if firstPage.Items[0].ID != first.ID || firstPage.Items[1].ID != second.ID {
		t.Fatalf("first page ids = [%s %s], want [%s %s]", firstPage.Items[0].ID, firstPage.Items[1].ID, first.ID, second.ID)
	}
	if firstPage.NextCursor == "" {
		t.Fatalf("first page nextCursor is empty, want cursor")
	}

	nextReq := httptest.NewRequest(http.MethodGet, "/notifications?status=FAILED&limit=2&cursor="+firstPage.NextCursor, nil)
	nextResp := httptest.NewRecorder()
	handler.ServeHTTP(nextResp, nextReq)
	if nextResp.Code != http.StatusOK {
		t.Fatalf("next list status = %d, want %d; body=%s", nextResp.Code, http.StatusOK, nextResp.Body.String())
	}
	var nextPage notificationListResponse
	if err := json.Unmarshal(nextResp.Body.Bytes(), &nextPage); err != nil {
		t.Fatalf("decode next list response: %v", err)
	}
	if len(nextPage.Items) != 1 {
		t.Fatalf("next page count = %d, want 1", len(nextPage.Items))
	}
	if nextPage.Items[0].ID != third.ID {
		t.Fatalf("next page id = %s, want %s", nextPage.Items[0].ID, third.ID)
	}
	if nextPage.NextCursor != "" {
		t.Fatalf("next page nextCursor = %q, want empty", nextPage.NextCursor)
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
