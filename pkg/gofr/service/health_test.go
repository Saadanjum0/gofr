package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"gofr.dev/pkg/gofr/logging"
	"gofr.dev/pkg/gofr/testutil"
)

func TestHTTPService_HealthCheck(t *testing.T) {
	service, server, metrics := initializeTest(t, "alive", http.StatusOK)
	defer server.Close()

	metrics.EXPECT().RecordHistogram(gomock.Any(), "app_http_service_response", gomock.Any(), "path", server.URL,
		"method", http.MethodGet, "status", fmt.Sprintf("%v", http.StatusOK)).Times(1)

	// when params value is of type []string then last value is sent in request
	resp := service.HealthCheck(t.Context())

	assert.Equal(t, &Health{Status: serviceUp, Details: map[string]any{"host": server.URL[7:]}},
		resp, "TEST[%d], Failed.\n%s")
}

func TestHTTPService_HealthCheckCustomURL(t *testing.T) {
	service, server, metrics := initializeTest(t, "ready", http.StatusOK)
	defer server.Close()

	metrics.EXPECT().RecordHistogram(gomock.Any(), "app_http_service_response", gomock.Any(), "path", server.URL,
		"method", http.MethodGet, "status", fmt.Sprintf("%v", http.StatusOK)).Times(1)

	// when params value is of type []string then last value is sent in request
	resp := service.HealthCheck(t.Context())

	assert.Equal(t, &Health{Status: serviceUp, Details: map[string]any{"host": server.URL[7:]}},
		resp, "TEST[%d], Failed.\n%s")
}

func TestHTTPService_HealthCheckErrorResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics := NewMockMetrics(ctrl)

	metrics.EXPECT().RecordHistogram(gomock.Any(), "app_http_service_response", gomock.Any(), "path", gomock.Any(),
		"method", http.MethodGet, "status", fmt.Sprintf("%v", http.StatusServiceUnavailable))

	service := NewHTTPService("http://test", logging.NewMockLogger(logging.INFO), metrics)

	// when params value is of type []string then last value is sent in request
	resp := service.HealthCheck(t.Context())

	body, _ := json.Marshal(&resp)

	assert.Contains(t, string(body), `{"status":"DOWN","details":{"error":"Get \"http://test/.well-known/alive\"`)
}

func TestHTTPService_HealthCheckDifferentStatusCode(t *testing.T) {
	service, server, metrics := initializeTest(t, "bad-request", http.StatusBadRequest)
	defer server.Close()

	metrics.EXPECT().RecordHistogram(gomock.Any(), "app_http_service_response", gomock.Any(), "path", server.URL,
		"method", http.MethodGet, "status", fmt.Sprintf("%v", http.StatusBadRequest)).AnyTimes()

	// when params value is of type []string then last value is sent in request
	resp := service.HealthCheck(t.Context())

	assert.Equal(t, &Health{Status: serviceDown,
		Details: map[string]any{"host": server.URL[7:], "error": serviceDownReason}},
		resp, "TEST[%d], Failed.\n%s")
}

func TestHTTPService_HealthCheckTimeout(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics := NewMockMetrics(ctrl)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/.well-known/alive", r.URL.Path)

		time.Sleep(1 * time.Second)
	}))

	metrics.EXPECT().RecordHistogram(gomock.Any(), "app_http_service_response", gomock.Any(), "path", server.URL,
		"method", http.MethodGet, "status", fmt.Sprintf("%v", http.StatusServiceUnavailable)).AnyTimes()

	log := testutil.StdoutOutputForFunc(func() {
		service := NewHTTPService(server.URL, logging.NewMockLogger(logging.INFO), metrics,
			&HealthConfig{HealthEndpoint: ".well-known/alive", Timeout: 1})

		resp := service.HealthCheck(t.Context())

		assert.Equal(t, &Health{Status: serviceDown,
			Details: map[string]any{"error": "Get \"" + server.URL + "/.well-known/alive\": context deadline exceeded"}},
			resp, "TEST[%d], Failed.\n%s")
	})

	assert.Contains(t, log, "context deadline exceeded")
}

func initializeTest(t *testing.T, urlSuffix string, statusCode int) (HTTP, *httptest.Server, *MockMetrics) {
	t.Helper()

	ctrl := gomock.NewController(t)
	metrics := NewMockMetrics(ctrl)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/.well-known/"+urlSuffix, r.URL.Path)

		if statusCode == http.StatusOK {
			_, _ = w.Write([]byte(`{"data":"UP"}`))
		}

		w.WriteHeader(statusCode)
	}))

	service := NewHTTPService(server.URL, logging.NewMockLogger(logging.INFO), metrics,
		&HealthConfig{HealthEndpoint: ".well-known/" + urlSuffix})

	return service, server, metrics
}

// A health endpoint that needs credentials or headers must receive the ones configured through the
// auth and DefaultHeaders options, or every check reports the service DOWN.
func TestHTTPService_HealthCheckSendsAuthAndDefaultHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" || r.Header.Get("X-Tenant") != "t" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	apiKey, err := NewAPIKeyConfig("k")
	require.NoError(t, err)

	svc := NewHTTPService(server.URL, logging.NewMockLogger(logging.INFO), nil,
		&HealthConfig{HealthEndpoint: "health"}, apiKey, &DefaultHeaders{Headers: map[string]string{"X-Tenant": "t"}})

	assert.Equal(t, serviceUp, svc.HealthCheck(t.Context()).Status)
}

// With a circuit breaker the health check decides when the circuit closes, so a check that cannot
// authenticate would keep it open for good.
func TestHTTPService_CircuitBreakerClosesWhenHealthEndpointNeedsAuth(t *testing.T) {
	var healed atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" || !healed.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	apiKey, err := NewAPIKeyConfig("k")
	require.NoError(t, err)

	svc := NewHTTPService(server.URL, logging.NewMockLogger(logging.INFO), nil,
		&HealthConfig{HealthEndpoint: "health"},
		&CircuitBreakerConfig{Threshold: 1, Interval: 20 * time.Millisecond}, apiKey)

	for range 2 {
		resp, _ := svc.Get(t.Context(), "data", nil)
		if resp != nil {
			resp.Body.Close()
		}
	}

	healed.Store(true)

	assert.Eventually(t, func() bool {
		resp, err := svc.Get(t.Context(), "data", nil)
		if resp != nil {
			resp.Body.Close()
		}

		return err == nil
	}, 3*time.Second, 20*time.Millisecond)
}
