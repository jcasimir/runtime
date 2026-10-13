package httpingress

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"miren.dev/runtime/api/ingress/ingress_v1alpha"
)

// newTimeoutTestServer builds the minimal Server needed to exercise the
// transport/timeout resolution helpers, skipping NewServer's RPC dependencies.
func newTimeoutTestServer(defaultTimeout time.Duration) *Server {
	return &Server{
		Log:        slog.New(slog.DiscardHandler),
		config:     IngressConfig{RequestTimeout: defaultTimeout},
		transport:  newProxyTransport(defaultTimeout),
		transports: make(map[time.Duration]http.RoundTripper),
	}
}

func TestRouteRequestTimeout(t *testing.T) {
	tests := []struct {
		name  string
		route *ingress_v1alpha.HttpRoute
		want  time.Duration
	}{
		{"nil route", nil, 0},
		{"unset", &ingress_v1alpha.HttpRoute{}, 0},
		{"minutes", &ingress_v1alpha.HttpRoute{RequestTimeout: "10m"}, 10 * time.Minute},
		{"seconds", &ingress_v1alpha.HttpRoute{RequestTimeout: "300s"}, 5 * time.Minute},
		{"unparseable", &ingress_v1alpha.HttpRoute{RequestTimeout: "10"}, 0},
		{"garbage", &ingress_v1alpha.HttpRoute{RequestTimeout: "forever"}, 0},
		{"zero", &ingress_v1alpha.HttpRoute{RequestTimeout: "0s"}, 0},
		{"negative", &ingress_v1alpha.HttpRoute{RequestTimeout: "-5m"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, routeRequestTimeout(tt.route))
		})
	}
}

func TestTransportForUsesDefault(t *testing.T) {
	h := newTimeoutTestServer(60 * time.Second)

	require.Same(t, h.transport, h.transportFor(0),
		"no override should reuse the shared default transport")
	require.Same(t, h.transport, h.transportFor(60*time.Second),
		"an override equal to the default should reuse the shared transport")
	require.Empty(t, h.transports, "no extra transports should have been built")
}

func TestTransportForMemoizesOverrides(t *testing.T) {
	h := newTimeoutTestServer(60 * time.Second)

	tenMin := h.transportFor(10 * time.Minute)
	require.NotSame(t, h.transport, tenMin, "an override needs its own transport")
	require.Same(t, tenMin, h.transportFor(10*time.Minute),
		"repeated lookups of one duration should return the same transport")

	fiveMin := h.transportFor(5 * time.Minute)
	require.NotSame(t, tenMin, fiveMin, "distinct durations need distinct transports")
	require.Len(t, h.transports, 2)
}

func TestTransportForAppliesOverrideTimeout(t *testing.T) {
	// Backend that never sends response headers.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer backend.Close()

	// A generous server default: without the override, this request would hang
	// well past the test's patience.
	h := newTimeoutTestServer(30 * time.Second)

	req, err := http.NewRequest(http.MethodGet, backend.URL+"/slow", nil)
	require.NoError(t, err)

	start := time.Now()
	//nolint:bodyclose // the round trip is expected to fail, so there is no body
	_, err = h.transportFor(100 * time.Millisecond).RoundTrip(req)
	elapsed := time.Since(start)

	require.Error(t, err, "the override timeout should have cut the request short")
	require.Less(t, elapsed, 5*time.Second,
		"request should have timed out on the override, not the 30s default")
}

func TestProxyTransportReusedConnectionGetsFreshTimeout(t *testing.T) {
	const timeout = 500 * time.Millisecond
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "payload" {
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()
	transport := newProxyTransport(timeout).(*http.Transport)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	resp, err := client.Get(backend.URL)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)

	// The next response fits the full budget, but not the old budget's remainder.
	time.Sleep(400 * time.Millisecond)
	req, err := http.NewRequest(http.MethodPost, backend.URL, strings.NewReader("payload"))
	require.NoError(t, err)
	var reused bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	}))
	start := time.Now()
	resp, err = client.Do(req)
	t.Logf("reused=%v, POST elapsed=%s, error=%v", reused, time.Since(start), err)
	require.True(t, reused, "the regression requires a warmed persistent connection")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "ok", string(body))
}

func TestProxyTransportResponseTimeouts(t *testing.T) {
	for _, mode := range []string{"silent headers", "stalled body", "active stream"} {
		t.Run(mode, func(t *testing.T) {
			const timeout = 400 * time.Millisecond
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "silent headers" {
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, "start\n")
				w.(http.Flusher).Flush()
				if mode == "stalled body" {
					<-r.Context().Done()
					return
				}
				for range 4 {
					time.Sleep(150 * time.Millisecond)
					_, _ = io.WriteString(w, "chunk\n")
					w.(http.Flusher).Flush()
				}
			}))
			defer backend.Close()
			transport := newProxyTransport(timeout).(*http.Transport)
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			start := time.Now()
			resp, err := client.Get(backend.URL)
			if mode != "silent headers" {
				require.NoError(t, err)
				defer resp.Body.Close()
				var body []byte
				body, err = io.ReadAll(resp.Body)
				if mode == "active stream" {
					require.NoError(t, err)
					require.Equal(t, "start\n"+strings.Repeat("chunk\n", 4), string(body))
					require.Greater(t, time.Since(start), timeout,
						"streaming must not have a total-duration cutoff")
					return
				}
				require.Equal(t, "start\n", string(body))
			}
			var netErr net.Error
			require.True(t, errors.As(err, &netErr), "expected network timeout, got %v", err)
			require.True(t, netErr.Timeout())
			require.GreaterOrEqual(t, time.Since(start), timeout/2,
				"must not time out prematurely")
			require.Less(t, time.Since(start), 3*timeout,
				"must be the ingress timeout, not the client's 5s safety limit")
		})
	}
}
