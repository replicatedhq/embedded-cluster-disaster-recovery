package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

type TokenReviewer interface {
	ReviewToken(context.Context, string, string, string) error
}

func Bootstrap(token string, next http.Handler) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		candidate, ok := bearerToken(request)
		if !ok || len(candidate) != len(expected) || subtle.ConstantTimeCompare([]byte(candidate), expected) != 1 {
			unauthorized(writer)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func InCluster(reviewer TokenReviewer, audience, serviceAccount string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token, ok := bearerToken(request)
		if !ok || reviewer.ReviewToken(request.Context(), token, audience, serviceAccount) != nil {
			unauthorized(writer)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func bearerToken(request *http.Request) (string, bool) {
	header := request.Header.Get("Authorization")
	if len(header) < 8 || !strings.EqualFold(header[:7], "Bearer ") {
		return "", false
	}
	token := header[7:]
	if token == "" || token != strings.TrimSpace(token) || strings.ContainsAny(token, "\r\n \t") {
		return "", false
	}
	return token, true
}

func unauthorized(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("WWW-Authenticate", "Bearer")
	writer.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(writer).Encode(map[string]any{"code": "unauthorized", "message": "lifecycle credential was rejected"})
}
