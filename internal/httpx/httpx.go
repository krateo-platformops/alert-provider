// Package httpx holds the HTTP error every outbound call of the alert provider reports.
package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// StatusError is a 4xx or 5xx answer. Its message is the one Python's requests raised, which the
// Alert and Incident statuses carry.
type StatusError struct {
	Code int
	URL  string
	// Throttled means the server refused the call for its rate limit (a 429, or what
	// provider-runtime's httpsignal reads as one); RetryAfter is then how long it asked the client
	// to wait, or the caller's default when it named no time. Only a client that reads the
	// rate-limit headers sets them.
	Throttled  bool
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	kind := "Client"
	if e.Code >= 500 {
		kind = "Server"
	}
	return fmt.Sprintf("%d %s Error: %s for url: %s", e.Code, kind, http.StatusText(e.Code), e.URL)
}

// Check is a StatusError for a 4xx or 5xx response, else nil.
func Check(resp *http.Response) error {
	if resp.StatusCode >= 400 && resp.StatusCode < 600 {
		return &StatusError{Code: resp.StatusCode, URL: resp.Request.URL.String()}
	}
	return nil
}

// Code is the status code of a StatusError in err's chain, or 0.
func Code(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// Throttled is the wait a throttled StatusError in err's chain asks for, and whether there is one.
func Throttled(err error) (time.Duration, bool) {
	var se *StatusError
	if errors.As(err, &se) && se.Throttled {
		return se.RetryAfter, true
	}
	return 0, false
}
