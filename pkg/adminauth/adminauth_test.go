package adminauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

var testSecret = []byte("unit-test-secret-0123456789abcdef")

func TestIssueAndParseToken(t *testing.T) {
	t.Run("system_admin round trip", func(t *testing.T) {
		raw, err := IssueToken(testSecret, "admin-1", RoleSystemAdmin, 0, time.Hour)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		claims, err := ParseToken(testSecret, raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if claims.Subject != "admin-1" || claims.Role != RoleSystemAdmin {
			t.Errorf("unexpected claims: %+v", claims)
		}
	})

	t.Run("dept_admin round trip", func(t *testing.T) {
		raw, err := IssueToken(testSecret, "dept-1", RoleDeptAdmin, 10002, time.Hour)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		claims, err := ParseToken(testSecret, raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if claims.TenantID != 10002 {
			t.Errorf("expected tenant 10002, got %d", claims.TenantID)
		}
	})

	t.Run("dept_admin requires tenant_id", func(t *testing.T) {
		if _, err := IssueToken(testSecret, "dept-1", RoleDeptAdmin, 0, time.Hour); err == nil {
			t.Fatal("expected error for dept_admin without tenant")
		}
	})

	t.Run("wrong secret rejected", func(t *testing.T) {
		raw, _ := IssueToken(testSecret, "admin-1", RoleSystemAdmin, 0, time.Hour)
		if _, err := ParseToken([]byte("wrong-secret-0123456789abcdef"), raw); err == nil {
			t.Fatal("expected signature error")
		}
	})

	t.Run("expired token rejected", func(t *testing.T) {
		raw, err := IssueToken(testSecret, "admin-1", RoleSystemAdmin, 0, -time.Minute)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		if _, err := ParseToken(testSecret, raw); err == nil {
			t.Fatal("expected expiration error")
		}
	})

	t.Run("non-HS256 algorithm rejected", func(t *testing.T) {
		claims := Claims{
			Role: RoleSystemAdmin,
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "admin-1",
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
		}
		raw, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString(testSecret)
		if err != nil {
			t.Fatalf("sign HS512: %v", err)
		}
		if _, err := ParseToken(testSecret, raw); err == nil {
			t.Fatal("expected algorithm rejection")
		}
	})

	t.Run("unknown role rejected", func(t *testing.T) {
		claims := Claims{
			Role: "superuser",
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "admin-1",
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
		}
		raw, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(testSecret)
		if _, err := ParseToken(testSecret, raw); err == nil {
			t.Fatal("expected role rejection")
		}
	})
}

type recordedAudit struct {
	events []model.AuditEvent
}

func (r *recordedAudit) record() AuditFunc {
	return func(_ context.Context, e *model.AuditEvent) error {
		r.events = append(r.events, *e)
		return nil
	}
}

func setupMiddlewareRouter(cfg MiddlewareConfig, withIDParam bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequireAdmin(cfg))
	if withIDParam {
		r.POST("/admin/api/spaces/:id/admins", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	} else {
		r.GET("/admin/api/overview", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	}
	return r
}

func doRequest(r *gin.Engine, method, path, auth, origin string) *httptest.ResponseRecorder {
	req, _ := http.NewRequest(method, path, strings.NewReader(""))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRequireAdmin_Middleware(t *testing.T) {
	t.Run("valid system_admin allowed and claims stored", func(t *testing.T) {
		rec := &recordedAudit{}
		r := setupMiddlewareRouter(MiddlewareConfig{Secret: testSecret, Audit: rec.record()}, false)

		raw, _ := IssueToken(testSecret, "admin-9", RoleSystemAdmin, 0, time.Hour)
		w := doRequest(r, http.MethodGet, "/admin/api/overview", raw, "")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		found := false
		for _, e := range rec.events {
			if e.Action == "admin.auth.allowed" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected allowed audit, got %+v", rec.events)
		}
	})

	t.Run("dept_admin scope enforced by path param", func(t *testing.T) {
		rec := &recordedAudit{}
		r := setupMiddlewareRouter(MiddlewareConfig{Secret: testSecret, Audit: rec.record()}, true)

		okToken, _ := IssueToken(testSecret, "dept-9", RoleDeptAdmin, 10002, time.Hour)
		if w := doRequest(r, http.MethodPost, "/admin/api/spaces/10002/admins", okToken, ""); w.Code != http.StatusOK {
			t.Fatalf("same-tenant should pass, got %d", w.Code)
		}
		if w := doRequest(r, http.MethodPost, "/admin/api/spaces/10003/admins", okToken, ""); w.Code != http.StatusForbidden {
			t.Fatalf("cross-tenant should be 403, got %d", w.Code)
		}
		if w := doRequest(r, http.MethodPost, "/admin/api/spaces/notanumber/admins", okToken, ""); w.Code != http.StatusForbidden {
			t.Fatalf("non-numeric id should be 403, got %d", w.Code)
		}
	})
}

func TestOriginMatching(t *testing.T) {
	allowed := normalizeOrigins([]string{
		"https://admin.example.com",
		"https://*.corp.example.com",
		"http://localhost:3000",
		"intranet.local",
	})

	cases := []struct {
		name   string
		origin string
		want   bool
	}{
		{"exact match", "https://admin.example.com", true},
		{"exact with default port", "https://admin.example.com:443", true},
		{"scheme mismatch", "http://admin.example.com", false},
		{"wildcard subdomain", "https://a.b.corp.example.com", true},
		{"wildcard base itself", "https://corp.example.com", true},
		{"wildcard other domain", "https://corp.example.org", false},
		{"localhost with port", "http://localhost:3000", true},
		{"localhost wrong port", "http://localhost:3001", false},
		{"bare host any scheme", "http://intranet.local", true},
		{"bare host https", "https://intranet.local", true},
		{"unrelated origin", "https://evil.example.com", false},
		{"garbage", "not a url", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := originAllowed(tc.origin, allowed); got != tc.want {
				t.Errorf("originAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}

	t.Run("empty whitelist rejects everything", func(t *testing.T) {
		if originAllowed("https://admin.example.com", nil) {
			t.Error("empty whitelist must reject all origins")
		}
	})
}

func TestParseServiceCredentialSharedToken(t *testing.T) {
	key := []byte("test-service-key-0123456789abcdef")
	jwtTok, err := IssueToken(key, "svc-jwt", RoleSystemAdmin, 0, 10*time.Minute)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	// JWT 形态仍可用
	if c, err := ParseServiceCredential(string(key), "shared-raw", jwtTok); err != nil || c.Role != RoleSystemAdmin {
		t.Fatalf("jwt path: %v %+v", err, c)
	}
	// 共享原始 token 形态
	if c, err := ParseServiceCredential(string(key), "shared-raw", "shared-raw"); err != nil || c.Role != RoleSystemAdmin {
		t.Fatalf("raw path: %v %+v", err, c)
	}
	// 错误值拒
	if _, err := ParseServiceCredential(string(key), "shared-raw", "nope"); err == nil {
		t.Fatal("garbage must be rejected")
	}
	// 未配置共享 token 时原始值拒
	if _, err := ParseServiceCredential(string(key), "", "shared-raw"); err == nil {
		t.Fatal("raw without configured shared token must be rejected")
	}
}
