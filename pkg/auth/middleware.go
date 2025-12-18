package auth

import (
	"context"
	"net/http"
	"strings"
)

// Middleware creates a new middleware for HTTP authentication
func Middleware(validator TokenValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth for login endpoint
			if r.URL.Path == "/api/auth/login" {
				next.ServeHTTP(w, r)
				return
			}

			// Get token from Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, "missing authorization header", http.StatusUnauthorized)
				return
			}

			tokenString := strings.TrimPrefix(authHeader, "Bearer ")
			claims, err := validator.ValidateToken(tokenString)
			if err != nil {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}

			// Check if token needs refresh
			shouldRefresh, err := ShouldRefreshToken(tokenString)
			if err == nil && shouldRefresh {
				newToken, err := RefreshToken(tokenString)
				if err == nil {
					// Add new token to response headers
					w.Header().Set("New-Token", newToken)
					w.Header().Set("Access-Control-Expose-Headers", "New-Token")
				}
			}

			// Add claims to context
			ctx := context.WithValue(r.Context(), "claims", claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
