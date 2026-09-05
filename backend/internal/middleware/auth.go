// Package middleware provides HTTP middleware for authentication and authorization
// This file handles Supabase JWT validation
package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// SupabaseJWTClaims represents the expected claims in a Supabase JWT token
type SupabaseJWTClaims struct {
	Sub               string `json:"sub"`                // User UUID from supabase.auth.users
	Email             string `json:"email"`              // User email
	Phone             string `json:"phone"`              // User phone (if applicable)
	AppMetadata       map[string]interface{} `json:"app_metadata"`
	UserMetadata      map[string]interface{} `json:"user_metadata"`
	Role              string `json:"role"`                // "authenticated" or "anon"
	AAL               string `json:"aal"`                 // Authentication Assurance Level
	AMR              []map[string]interface{} `json:"amr"` // Authentication Methods References
	SessionID         string `json:"session_id"`           // Session ID for tracking
	IsAnonymous       bool   `json:"is_anonymous"`         // Whether this is an anonymous user

	// Custom claims we add after authentication (not in original JWT)
	PharmacyID       string `json:"pharmacy_id,omitempty"`       // Will be populated from DB
	BranchID         string `json:"branch_id,omitempty"`         // Will be populated from DB
	EmployeeID       string `json:"employee_id,omitempty"`       // Will be populated from DB
	PermissionVersion int    `json:"permission_version,omitempty"` // For cache invalidation

	jwt.RegisteredClaims
}

// AuthConfig holds configuration for the auth middleware
type AuthConfig struct {
	JWTSecret    string
	SupabaseURL  string
	TokenLookup  string // Default: "header:Authorization"
	TimeSkew     time.Duration // Allowable clock skew (default: 30 seconds)
}

// DefaultAuthConfig returns default auth configuration
func DefaultAuthConfig(jwtSecret, supabaseURL string) *AuthConfig {
	return &AuthConfig{
		JWTSecret:   jwtSecret,
		SupabaseURL: supabaseURL,
		TokenLookup: "header:Authorization",
		TimeSkew:    30 * time.Second,
	}
}

// SupabaseAuth creates a middleware that validates Supabase JWT tokens
// This middleware:
// 1. Extracts Bearer token from Authorization header
// 2. Validates the token signature using Supabase JWT secret
// 3. Extracts claims (sub, email, role, etc.)
// 4. Sets user context for downstream handlers
func SupabaseAuth(config *AuthConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenString, err := extractToken(c, config.TokenLookup)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":   "authentication_required",
				"message": err.Error(),
				"code":    "MISSING_OR_INVALID_TOKEN",
			})
			return
		}

		// Parse and validate the JWT token
		claims, err := validateSupabaseToken(tokenString, config)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":   "invalid_token",
				"message": err.Error(),
				"code":    "INVALID_TOKEN",
			})
			return
		}

		// Set claims in Gin context for downstream middleware/handlers
		setUserContext(c, claims)

		// Add request ID for tracing (can be used in audit logs)
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = generateRequestID()
		}
		c.Set("request_id", requestID)

		c.Next()
	}
}

// OptionalAuth creates a middleware that validates token if present but doesn't require it
// Useful for endpoints that work for both authenticated and unauthenticated users
func OptionalAuth(config *AuthConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenString, err := extractToken(c, config.TokenLookup)
		if err != nil {
			// No token present - continue as unauthenticated
			c.Set("authenticated", false)
			c.Next()
			return
		}

		claims, err := validateSupabaseToken(tokenString, config)
		if err != nil {
			// Invalid token - continue as unauthenticated (don't block)
			c.Set("authenticated", false)
			c.Next()
			return
		}

		// Valid token found
		setUserContext(c, claims)
		c.Set("authenticated", true)
		c.Next()
	}
}

// extractToken extracts the JWT token from the request based on TokenLookup format
// Format: "header:<name>" or "query:<param>" or "cookie:<name>"
func extractToken(c *gin.Context, tokenLookup string) (string, error) {
	parts := strings.SplitN(tokenLookup, ":", 2)
	if len(parts) != 2 {
		return "", errors.New("invalid token lookup format")
	}

	source := parts[0]
	name := parts[1]

	switch source {
	case "header":
		authHeader := c.GetHeader(name)
		if authHeader == "" {
			return "", errors.New("missing authorization header")
		}

		// Support both "Bearer <token>" and raw token formats
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token == "" {
				return "", errors.New("empty bearer token")
			}
			return token, nil
		}

		// Allow raw token (for flexibility)
		return authHeader, nil

	case "query":
		token := c.Query(name)
		if token == "" {
			return "", fmt.Errorf("missing query parameter: %s", name)
		}
		return token, nil

	case "cookie":
		token, err := c.Cookie(name)
		if err != nil {
			return "", fmt.Errorf("missing cookie: %s", name)
		}
		return token, nil

	default:
		return "", fmt.Errorf("unsupported token source: %s", source)
	}
}

// validateSupabaseToken parses and validates a Supabase JWT token
// Returns parsed claims or an error if validation fails
func validateSupabaseToken(tokenString string, config *AuthConfig) (*SupabaseJWTClaims, error) {
	// Parse the token with custom claims
	token, err := jwt.ParseWithClaims(tokenString, &SupabaseJWTClaims{}, func(token *jwt.Token) (interface{}, error) {
		// Validate signing method (Supabase uses HS256)
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		// Return the secret key for validation
		return []byte(config.JWTSecret), nil
	})

	if err != nil {
		// Provide more specific error messages
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, errors.New("token has expired")
		}
		if errors.Is(err, jwt.ErrTokenNotValidYet) {
			return nil, errors.New("token is not valid yet")
		}
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	if !token.Valid {
		return nil, errors.New("token is invalid")
	}

	// Extract and validate claims
	claims, ok := token.Claims.(*SupabaseJWTClaims)
	if !ok {
		return nil, errors.New("failed to extract token claims")
	}

	// Validate required claims
	if claims.Sub == "" {
		return nil, errors.New("missing subject claim (sub)")
	}

	// Validate token is not expired (with allowed time skew)
	if claims.ExpiresAt != nil {
		if time.Now().Add(config.TimeSkew).Before(claims.ExpiresAt.Time) {
			// Token is still valid (with time skew consideration)
		} else {
			return nil, errors.New("token has expired")
		}
	}

	// Validate issued-at time (prevent tokens from the future)
	if claims.IssuedAt != nil {
		if claims.IssuedAt.Time.After(time.Now().Add(config.TimeSkew)) {
			return nil, errors.New("token issued in the future (clock skew issue)")
		}
	}

	// Validate role (should be "authenticated" or a valid service role)
	if claims.Role != "authenticated" && claims.Role != "service_role" && claims.Role != "anon" {
		return nil, fmt.Errorf("unexpected token role: %s", claims.Role)
	}

	return claims, nil
}

// setUserContext sets the authenticated user's information in the Gin context
// This makes user data available to all downstream handlers and middleware
func setUserContext(c *gin.Context, claims *SupabaseJWTClaims) {
	// Set individual claims for easy access
	c.Set("user_id", claims.Sub) // Supabase auth user ID
	c.Set("auth_user_id", claims.Sub)
	c.Set("email", claims.Email)
	c.Set("phone", claims.Phone)
	c.Set("role", claims.Role) // "authenticated", "anon", or "service_role"
	c.Set("session_id", claims.SessionID)
	c.Set("is_anonymous", claims.IsAnonymous)
	
	// Set app_metadata and user_metadata (useful for frontend)
	c.Set("app_metadata", claims.AppMetadata)
	c.Set("user_metadata", claims.UserMetadata)
	
	// Set full claims object for advanced use cases
	c.Set("jwt_claims", claims)
	
	// Set context with typed values for Go handlers
	ctx := context.WithValue(c.Request.Context(), UserIDKey, claims.Sub)
	ctx = context.WithValue(ctx, EmailKey, claims.Email)
	ctx = context.WithValue(ctx, RoleKey, claims.Role)
	ctx = context.WithValue(ctx, SessionIDKey, claims.SessionID)
	ctx = context.WithValue(ctx, ClaimsKey, claims)
	
	c.Request = c.Request.WithContext(ctx)
}

// Context keys for typed context values
type contextKey string

const (
	UserIDKey    contextKey = "user_id"
	EmailKey     contextKey = "email"
	RoleKey      contextKey = "role"
	SessionIDKey contextKey = "session_id"
	ClaimsKey    contextKey = "jwt_claims"
)

// GetUserID extracts user ID from Gin context
func GetUserID(c *gin.Context) string {
	if userID, exists := c.Get("user_id"); exists {
		if id, ok := userID.(string); ok {
			return id
		}
	}
	return ""
}

// GetEmail extracts email from Gin context
func GetEmail(c *gin.Context) string {
	if email, exists := c.Get("email"); exists {
		if e, ok := email.(string); ok {
			return e
		}
	}
	return ""
}

// GetSessionID extracts session ID from Gin context
func GetSessionID(c *gin.Context) string {
	if sessionID, exists := c.Get("session_id"); exists {
		if sid, ok := sessionID.(string); ok {
			return sid
		}
	}
	return ""
}

// GetJWTClaims extracts full JWT claims from Gin context
func GetJWTClaims(c *gin.Context) (*SupabaseJWTClaims, bool) {
	claims, exists := c.Get("jwt_claims")
	if !exists {
		return nil, false
	}
	
	jwtClaims, ok := claims.(*SupabaseJWTClaims)
	return jwtClaims, ok
}

// IsAuthenticated checks if current request is authenticated
func IsAuthenticated(c *gin.Context) bool {
	if authenticated, exists := c.Get("authenticated"); exists {
		if auth, ok := authenticated.(bool); ok {
			return auth
		}
	}
	// If authenticated flag not set, check if user_id exists
	return GetUserID(c) != ""
}

// generateRequestID generates a unique request ID for tracing
func generateRequestID() string {
	// Simple UUID-like generation (in production, use proper UUID library)
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().UnixMicro())
}

// AuthResponse represents a standardized authentication response
type AuthResponse struct {
	User        UserInfo `json:"user"`
	AccessToken string   `json:"access_token,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}

// UserInfo represents basic user information returned after auth
type UserInfo struct {
	ID            string                 `json:"id"`
	Email         string                 `json:"email"`
	Phone         string                 `json:"phone,omitempty"`
	Role          string                 `json:"role"`
	EmployeeID    string                 `json:"employee_id,omitempty"`
	PharmacyID    string                 `json:"pharmacy_id,omitempty"`
	BranchID      string                 `json:"branch_id,omitempty"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
}

// MarshalJSON custom JSON marshaler for cleaner output
func (u *UserInfo) MarshalJSON() ([]byte, error) {
	type Alias UserInfo
	return json.Marshal(&struct {
		*Alias
		Phone string `json:"phone,omitempty"`
	}{
		Alias: (*Alias)(u),
		Phone: func() string { 
			if u.Phone != "" { return u.Phone } 
			return "" 
		}(),
	})
}
