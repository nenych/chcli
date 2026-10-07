package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// parseJWTClaims decodes the payload of a JWT without verifying its
// signature. Use it only for display purposes or on tokens whose origin is
// already trusted (verified separately, or received over TLS from the token
// endpoint).
func parseJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, errors.New("not a JWT")
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errors.New("not a JWT")
	}
	return claims, nil
}

// claimString returns the first non-empty string claim among names.
func claimString(claims map[string]any, names ...string) string {
	for _, name := range names {
		if name == "" {
			continue
		}
		if s, ok := claims[name].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// claimTime reads a NumericDate claim such as "exp".
func claimTime(claims map[string]any, name string) time.Time {
	if f, ok := claims[name].(float64); ok && f > 0 {
		return time.Unix(int64(f), 0)
	}
	return time.Time{}
}
