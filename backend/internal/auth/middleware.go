package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	AccessCookieName  = "pharmacy_access"
	RefreshCookieName = "pharmacy_refresh"
	CSRFCookieName    = "pharmacy_csrf"
	CSRFHeaderName    = "X-CSRF-Token"
)

type contextKey string

const principalContextKey contextKey = "auth_principal"

// Middleware authenticates requests using the short-lived opaque access cookie.
// Authorization Bearer is retained for server-to-server clients, not browsers.
func (s *Service) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := accessTokenFromRequest(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "authentication_required", "message": "Authentication required",
			})
			return
		}
		principal, err := s.Authenticate(c.Request.Context(), token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "invalid_session", "message": "Session expired or invalid",
			})
			return
		}
		setPrincipal(c, principal)
		c.Next()
	}
}

// RequirePharmacyPrincipal allows an authenticated company owner or pharmacy
// employee to access the pharmacy attached to the current principal. The
// pharmacy scope is always derived from the session, never from the request.
func RequirePharmacyPrincipal() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := PrincipalFromContext(c)
		if !ok ||
			(principal.Type != EmployeePrincipal && principal.Type != CompanyUserPrincipal) ||
			principal.PharmacyID == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":   "pharmacy_account_required",
				"message": "An active pharmacy account is required",
			})
			return
		}
		c.Next()
	}
}

// RequireEmployeePrincipal restricts sensitive pharmacy operations to employee
// identities. Company users can read the pharmacy dashboard through
// RequirePharmacyPrincipal, but they do not automatically gain employee-only
// mutation privileges.
func RequireEmployeePrincipal() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := PrincipalFromContext(c)
		if !ok || principal.Type != EmployeePrincipal || principal.PharmacyID == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":   "pharmacy_employee_required",
				"message": "An active pharmacy employee account is required",
			})
			return
		}
		c.Next()
	}
}

// CSRF protects cookie-authenticated state-changing requests with a
// double-submit token. GET, HEAD and OPTIONS remain safe without the header.
func CSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		cookie, err := c.Cookie(CSRFCookieName)
		header := c.GetHeader(CSRFHeaderName)
		if err != nil || cookie == "" || header == "" || cookie != header {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "csrf_failed", "message": "CSRF validation failed",
			})
			return
		}
		c.Next()
	}
}

func setPrincipal(c *gin.Context, principal *Principal) {
	c.Set(string(principalContextKey), principal)
	c.Set("user_id", principal.ID)
	c.Set("auth_user_id", principal.ID)
	c.Set("email", principal.Email)
	c.Set("role", principal.Role)
	c.Set("principal_type", principal.Type)
	c.Set("company_id", principal.CompanyID)
	c.Set("pharmacy_id", principal.PharmacyID)
	c.Set("branch_id", principal.BranchID)
	c.Set("permission_version", principal.PermissionVersion)
}

func PrincipalFromContext(c *gin.Context) (*Principal, bool) {
	value, exists := c.Get(string(principalContextKey))
	if !exists {
		return nil, false
	}
	principal, ok := value.(*Principal)
	return principal, ok
}

func accessTokenFromRequest(c *gin.Context) string {
	if value, err := c.Cookie(AccessCookieName); err == nil && value != "" {
		return value
	}
	header := strings.TrimSpace(c.GetHeader("Authorization"))
	if strings.HasPrefix(header, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	}
	return ""
}

func refreshTokenFromRequest(c *gin.Context) string {
	value, _ := c.Cookie(RefreshCookieName)
	return value
}

func newCSRFToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
