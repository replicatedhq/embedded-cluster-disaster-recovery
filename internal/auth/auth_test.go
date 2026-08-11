package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type reviewerFunc func(context.Context, string, string, string) error

func (f reviewerFunc) ReviewToken(ctx context.Context, token, audience, serviceAccount string) error {
	return f(ctx, token, audience, serviceAccount)
}

func TestBootstrap(t *testing.T) {
	handler := Bootstrap("0123456789abcdef0123456789abcdef", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	for _, test := range []struct {
		name, authorization string
		want                int
	}{
		{name: "accepted", authorization: "Bearer 0123456789abcdef0123456789abcdef", want: http.StatusNoContent},
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", authorization: "Bearer 0123456789abcdef0123456789abcdeg", want: http.StatusUnauthorized},
		{name: "spaces", authorization: "Bearer 0123456789abcdef 123456789abcdef", want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Authorization", test.authorization)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestInClusterPassesBoundIdentityRequirements(t *testing.T) {
	called := false
	reviewer := reviewerFunc(func(_ context.Context, token, audience, identity string) error {
		called = true
		if token != "short-lived-token" || audience != "lifecycle" || identity != "system:serviceaccount:dr:handler" {
			t.Fatalf("unexpected review input: %q %q %q", token, audience, identity)
		}
		return nil
	})
	handler := InCluster(reviewer, "lifecycle", "system:serviceaccount:dr:handler", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer short-lived-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !called {
		t.Fatalf("status = %d, reviewer called = %v", response.Code, called)
	}
}
