package clientconfig

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"miren.dev/runtime/pkg/cloudauth"
)

func resetTokenMemo(t *testing.T) {
	t.Helper()
	reset := func() {
		tokenMemo.Lock()
		tokenMemo.tokens = map[string]string{}
		tokenMemo.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// fakeCloudAuth serves the user-key challenge-response endpoints and counts
// how many handshakes it was asked to begin.
func fakeCloudAuth(t *testing.T, token string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var begins atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/user-key/begin", func(w http.ResponseWriter, r *http.Request) {
		begins.Add(1)
		_ = json.NewEncoder(w).Encode(BeginAuthResponse{
			Envelope:  "envelope",
			Challenge: base64.URLEncoding.EncodeToString([]byte("challenge")),
		})
	})
	mux.HandleFunc("/auth/user-key/complete", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(CompleteAuthResponse{Token: token})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &begins
}

// keyWithSlash generates key pairs until one has a '/' in its fingerprint,
// which is the shape that used to defeat the cache. About half of them do.
func keyWithSlash(t *testing.T) *cloudauth.KeyPair {
	t.Helper()
	for range 100 {
		kp, err := cloudauth.GenerateKeyPair()
		require.NoError(t, err)
		if strings.Contains(kp.Fingerprint(), "/") {
			return kp
		}
	}
	t.Fatal("no fingerprint with a '/' in 100 tries")
	return nil
}

func TestTokenCacheKey(t *testing.T) {
	fp := "SHA256:fazGCLp3abc/def+ghi/LuSrgBf4"

	key := tokenCacheKey("https://miren.cloud", fp)
	require.NotContains(t, key, "/")
	require.Equal(t, key, filepath.Base(key))
	require.True(t, strings.HasPrefix(key, "token_"))

	require.Equal(t, key, tokenCacheKey("https://miren.cloud", fp), "keys must be stable")
	require.NotEqual(t, key, tokenCacheKey("http://miren.host:3001", fp),
		"one key used against two clouds must not share a cache entry")
}

func TestAuthenticateWithKeyCachesTokenOnDisk(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetTokenMemo(t)

	token := makeToken(t, jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()})
	srv, begins := fakeCloudAuth(t, token)
	kp := keyWithSlash(t)

	got, err := AuthenticateWithKey(context.Background(), slog.Default(), srv.URL, kp)
	require.NoError(t, err)
	require.Equal(t, token, got)
	require.EqualValues(t, 1, begins.Load())

	// A later command is a new process with an empty memo, so it only gets the
	// token back if it made it to disk.
	resetTokenMemo(t)

	got, err = AuthenticateWithKey(context.Background(), slog.Default(), srv.URL, kp)
	require.NoError(t, err)
	require.Equal(t, token, got)
	require.EqualValues(t, 1, begins.Load(), "the second call should be served from the disk cache")
}

func TestAuthenticateWithKeyMemoizesWithoutDiskCache(t *testing.T) {
	// A HOME we can't create ~/.cache under: every disk cache write fails.
	home := filepath.Join(t.TempDir(), "home")
	require.NoError(t, os.WriteFile(home, nil, 0600))
	t.Setenv("HOME", home)
	resetTokenMemo(t)

	token := makeToken(t, jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()})
	srv, begins := fakeCloudAuth(t, token)
	kp := keyWithSlash(t)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	for range 3 {
		got, err := AuthenticateWithKey(context.Background(), log, srv.URL, kp)
		require.NoError(t, err)
		require.Equal(t, token, got)
	}
	require.EqualValues(t, 1, begins.Load(), "one command should do one handshake even with no disk cache")
	require.Contains(t, logs.String(), "failed to cache cloud token",
		"a cache that can't be written should be visible in the logger we were handed")
}

func TestAuthenticateWithKeyRateLimited(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetTokenMemo(t)

	reset := time.Now().Add(23 * time.Second).Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
	}))
	t.Cleanup(srv.Close)

	kp, err := cloudauth.GenerateKeyPair()
	require.NoError(t, err)

	_, err = AuthenticateWithKey(context.Background(), slog.Default(), srv.URL, kp)
	rl, ok := errors.AsType[*CloudRateLimitError](err)
	require.True(t, ok, "got %v, want a CloudRateLimitError", err)
	require.True(t, rl.RetryAt.Equal(reset), "RetryAt = %v, want %v", rl.RetryAt, reset)
}

func TestRetryAt(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 35, 57, 0, time.UTC)

	cases := []struct {
		name    string
		headers map[string]string
		want    time.Time
	}{
		{"nothing", nil, time.Time{}},
		{"reset timestamp", map[string]string{"X-RateLimit-Reset": strconv.FormatInt(now.Add(20*time.Second).Unix(), 10)}, now.Add(20 * time.Second)},
		{"retry-after seconds", map[string]string{"Retry-After": "7"}, now.Add(7 * time.Second)},
		{"retry-after wins over reset", map[string]string{
			"Retry-After":       "7",
			"X-RateLimit-Reset": strconv.FormatInt(now.Add(20*time.Second).Unix(), 10),
		}, now.Add(7 * time.Second)},
		{"retry-after http date", map[string]string{"Retry-After": now.Add(time.Minute).Format(http.TimeFormat)}, now.Add(time.Minute)},
		{"garbage", map[string]string{"Retry-After": "soon", "X-RateLimit-Reset": "later"}, time.Time{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			got := retryAt(h, now)
			require.True(t, got.Equal(tc.want), "got %v, want %v", got, tc.want)
		})
	}
}

func TestCloudRateLimitErrorRetryIn(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 35, 57, 0, time.UTC)

	require.Zero(t, (&CloudRateLimitError{}).RetryIn(now))
	require.Zero(t, (&CloudRateLimitError{RetryAt: now.Add(-time.Second)}).RetryIn(now))
	require.Equal(t, 23*time.Second, (&CloudRateLimitError{RetryAt: now.Add(22*time.Second + 300*time.Millisecond)}).RetryIn(now))
}

func TestAuthenticateWithKeyRemovesLegacyCacheEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetTokenMemo(t)

	var kp *cloudauth.KeyPair
	for range 100 {
		k, err := cloudauth.GenerateKeyPair()
		require.NoError(t, err)
		if !strings.Contains(k.Fingerprint(), "/") {
			kp = k
			break
		}
	}
	require.NotNil(t, kp, "no fingerprint without a '/' in 100 tries")

	// What an older CLI left behind: a still-fresh token named by fingerprint.
	legacy := filepath.Join(home, ".cache", "miren", kp.Fingerprint())
	require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0700))
	old := makeToken(t, jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()})
	require.NoError(t, os.WriteFile(legacy, []byte(old), 0600))

	token := makeToken(t, jwt.MapClaims{"exp": time.Now().Add(2 * time.Hour).Unix()})
	srv, begins := fakeCloudAuth(t, token)

	got, err := AuthenticateWithKey(context.Background(), slog.Default(), srv.URL, kp)
	require.NoError(t, err)
	require.Equal(t, token, got, "the legacy entry must not be read")
	require.EqualValues(t, 1, begins.Load())
	require.NoFileExists(t, legacy)
}
