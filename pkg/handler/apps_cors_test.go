package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAppsDesktopCORS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	apps := r.Group("/api/v1/apps", AppsDesktopCORS())
	apps.OPTIONS("/*path", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	apps.GET("", func(c *gin.Context) { c.Status(http.StatusOK) })
	for _, tc := range []struct {
		method, path, origin string
		status               int
		allow                string
	}{
		{http.MethodOptions, "/api/v1/apps/check-slug", "tauri://localhost", 204, "tauri://localhost"},
		{http.MethodGet, "/api/v1/apps", "http://tauri.localhost", 200, "http://tauri.localhost"},
		{http.MethodOptions, "/api/v1/apps/publish", "https://untrusted.example", 403, ""},
		{http.MethodGet, "/api/v1/apps", "", 200, ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status || w.Header().Get("Access-Control-Allow-Origin") != tc.allow {
			t.Fatalf("%s %s origin=%q: status=%d allow=%q", tc.method, tc.path, tc.origin, w.Code, w.Header().Get("Access-Control-Allow-Origin"))
		}
	}
}
