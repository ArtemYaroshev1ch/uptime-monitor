package notifier

import (
	"errors"
	"fmt"
	"net/http"
)

// permanentError marks a delivery failure that retrying cannot fix: a chat
// that does not exist, a webhook URL that is blocked, a channel type the
// server is not configured for. The notifier fails such a notification at
// once instead of spending all its attempts.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent wraps err so that IsPermanent reports true.
func Permanent(err error) error { return permanentError{err: err} }

// IsPermanent reports whether retrying the delivery is pointless.
func IsPermanent(err error) bool {
	_, ok := errors.AsType[permanentError](err)
	return ok
}

// statusError turns a non-2xx response into an error. Server errors, 408
// and 429 are worth retrying; any other 4xx means the request itself is
// wrong (bad chat ID, missing endpoint) and will stay wrong.
func statusError(service string, code int, detail string) error {
	err := fmt.Errorf("%s responded %d %s", service, code, http.StatusText(code))
	if detail != "" {
		err = fmt.Errorf("%w: %s", err, detail)
	}
	retryable := code >= 500 || code == http.StatusRequestTimeout || code == http.StatusTooManyRequests
	if !retryable {
		return Permanent(err)
	}
	return err
}

// maxDetail bounds the part of a response body quoted in an error.
const maxDetail = 200

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary so the stored text stays valid UTF-8.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "..."
}
