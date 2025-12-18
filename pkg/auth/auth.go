package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	jwtSecret = []byte("m7s_secret_key") // In production, this should be properly configured
	tokenTTL  = 24 * time.Hour
	// Add refresh threshold - refresh token if it expires in less than 30 minutes
	refreshThreshold = 30 * time.Minute
)

// JWTClaims represents the JWT claims
type JWTClaims struct {
	Username string `json:"username"`
}

// TokenValidator is an interface for token validation
type TokenValidator interface {
	ValidateToken(tokenString string) (*JWTClaims, error)
}

// GenerateToken generates a new JWT token for a user
func GenerateToken(username string) (string, error) {
	claims := jwt.RegisteredClaims{
		Subject:   username,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(tokenTTL)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		NotBefore: jwt.NewNumericDate(time.Now()),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

// ValidateJWT validates a JWT token and returns the claims
func ValidateJWT(tokenString string) (*JWTClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &jwt.RegisteredClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return jwtSecret, nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*jwt.RegisteredClaims); ok && token.Valid {
		return &JWTClaims{Username: claims.Subject}, nil
	}

	return nil, errors.New("invalid token")
}

// ShouldRefreshToken checks if a token should be refreshed based on its expiration time
func ShouldRefreshToken(tokenString string) (bool, error) {
	token, err := jwt.ParseWithClaims(tokenString, &jwt.RegisteredClaims{}, func(token *jwt.Token) (interface{}, error) {
		return jwtSecret, nil
	})
	if err != nil {
		return false, err
	}

	if claims, ok := token.Claims.(*jwt.RegisteredClaims); ok && token.Valid {
		if claims.ExpiresAt != nil {
			timeUntilExpiry := time.Until(claims.ExpiresAt.Time)
			return timeUntilExpiry < refreshThreshold, nil
		}
	}
	return false, errors.New("invalid token")
}

// RefreshToken validates the old token and generates a new one if it's still valid
func RefreshToken(oldToken string) (string, error) {
	claims, err := ValidateJWT(oldToken)
	if err != nil {
		return "", err
	}
	return GenerateToken(claims.Username)
}
