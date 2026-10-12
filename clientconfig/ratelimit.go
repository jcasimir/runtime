package clientconfig

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// CloudRateLimitError reports that the cloud refused a sign-in because this
// client has made too many of them recently. Signing in again won't help, so
// callers should surface RetryAt instead of pointing the user at `miren login`.
type CloudRateLimitError struct {
	// RetryAt is when the cloud says it will accept another attempt. It is
	// zero when the response didn't say.
	RetryAt time.Time
}

func (e *CloudRateLimitError) Error() string {
	if e.RetryAt.IsZero() {
		return "cloud rate limit exceeded"
	}
	return fmt.Sprintf("cloud rate limit exceeded; retry after %s", e.RetryAt.Format(time.RFC3339))
}

// RetryIn returns how long until RetryAt, rounded up to a whole second and
// never negative. It is zero when RetryAt is unknown.
func (e *CloudRateLimitError) RetryIn(now time.Time) time.Duration {
	if e.RetryAt.IsZero() {
		return 0
	}
	d := e.RetryAt.Sub(now)
	if d <= 0 {
		return 0
	}
	return d.Truncate(time.Second) + time.Second
}

// authResponseError turns a non-200 response from the cloud's auth endpoints
// into an error, keeping the retry timing from a 429.
func authResponseError(resp *http.Response, body []byte) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		return &CloudRateLimitError{RetryAt: retryAt(resp.Header, time.Now())}
	}

	var errResp map[string]string
	_ = json.Unmarshal(body, &errResp)
	if errMsg, ok := errResp["error"]; ok {
		return fmt.Errorf("authentication failed: %s", errMsg)
	}
	return fmt.Errorf("server returned status %d: %s", resp.StatusCode, string(body))
}

// retryAt reads when a rate-limited request may be retried. Retry-After is
// the standard header and wins when present; cloud's limiter has historically
// sent only X-RateLimit-Reset, a unix timestamp.
func retryAt(h http.Header, now time.Time) time.Time {
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return now.Add(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(v); err == nil {
			return t
		}
	}
	if v := h.Get("X-RateLimit-Reset"); v != "" {
		if unix, err := strconv.ParseInt(v, 10, 64); err == nil && unix > 0 {
			return time.Unix(unix, 0)
		}
	}
	return time.Time{}
}
