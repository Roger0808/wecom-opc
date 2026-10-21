package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCodeInvalidateChannelNotify(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := &CodeInvalidateChannel{BaseURL: srv.URL, ServiceToken: "svc-secret", HTTPClient: srv.Client()}
	ev := InvalidateEvent{Subject: "sub-1", TenantID: "10001", Reason: "revoke", Version: 7, At: time.Now()}
	if err := ch.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotAuth != "Bearer svc-secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotBody["enterprise_subject"] != "sub-1" || gotBody["version"] != float64(7) {
		t.Fatalf("body = %v", gotBody)
	}
}

func TestCodeInvalidateChannelNotifyFailures(t *testing.T) {
	// Unconfigured channel refuses (Notifier keeps the event in the retry queue).
	ch := &CodeInvalidateChannel{}
	if err := ch.Notify(context.Background(), InvalidateEvent{Subject: "s"}); err == nil {
		t.Fatal("unconfigured channel must fail")
	}
	// Non-2xx surfaces as an error so the retry queue keeps the event.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ch = &CodeInvalidateChannel{BaseURL: srv.URL, HTTPClient: srv.Client()}
	if err := ch.Notify(context.Background(), InvalidateEvent{Subject: "s"}); err == nil {
		t.Fatal("non-2xx must fail")
	}
}
