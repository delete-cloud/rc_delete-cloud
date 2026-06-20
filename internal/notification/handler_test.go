package notification

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerCreateAndGetNotification(t *testing.T) {
	store := newTestStore(t)
	handler := NewHandler(store)
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
