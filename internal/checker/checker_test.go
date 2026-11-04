package checker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
)

// fakeResolver answers from a map; unknown names are NXDOMAIN.
type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if addrs, ok := f[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

var loopback = []netip.Addr{netip.MustParseAddr("127.0.0.1")}

func target(rawURL string) domain.CheckTarget {
	return domain.CheckTarget{
		MonitorID:       uuid.New(),
		URL:             rawURL,
		Method:          http.MethodGet,
		Timeout:         5 * time.Second,
		FollowRedirects: true,
	}
}

// newServer starts an httptest server whose error log is discarded (failed
// TLS handshakes are expected in some tests).
func newServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// hostPort replaces the host of srv's URL, keeping the port.
func withHost(t *testing.T, srvURL, host string) string {
	t.Helper()
	u, err := url.Parse(srvURL)
	require.NoError(t, err)
	u.Host = net.JoinHostPort(host, u.Port())
	return u.String()
}

func TestCheckSuccess(t *testing.T) {
	t.Parallel()
	var userAgent string
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.UserAgent()
		_, _ = io.WriteString(w, "hello world")
	}))
	c := New(Options{AllowPrivate: true, UserAgent: "uptime-monitor-test/1.0"})

	res := c.Check(t.Context(), target(srv.URL))

	assert.True(t, res.Success, res.ErrorMessage)
	assert.Empty(t, res.ErrorKind)
	require.NotNil(t, res.StatusCode)
	assert.Equal(t, http.StatusOK, *res.StatusCode)
	assert.Equal(t, "HTTP/1.1", res.HTTPProto)
	assert.Nil(t, res.DNSMs, "an IP literal needs no lookup")
	assert.NotNil(t, res.ConnectMs)
	assert.Nil(t, res.TLSMs, "plain HTTP")
	assert.NotNil(t, res.TTFBMs)
	assert.Empty(t, res.TLSVersion)
	assert.Nil(t, res.TLSNotAfter)
	assert.GreaterOrEqual(t, res.ResponseTimeMs, 0)
	assert.WithinDuration(t, time.Now(), res.CheckedAt, time.Minute)
	assert.Equal(t, "uptime-monitor-test/1.0", userAgent)
}

func TestCheckResponseRules(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", MaxBodyBytes) + "needle"
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/500":
			w.WriteHeader(http.StatusInternalServerError)
		case "/204":
			w.WriteHeader(http.StatusNoContent)
		case "/big":
			_, _ = io.WriteString(w, big)
		case "/head":
			if r.Method != http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		default:
			_, _ = io.WriteString(w, "Example Domain")
		}
	}))
	c := New(Options{AllowPrivate: true})

	tests := []struct {
		name     string
		path     string
		method   string
		expected *int
		keyword  *string
		wantKind domain.ErrorKind
		wantCode int
	}{
		{name: "200 is any 2xx", path: "/", wantCode: 200},
		{name: "204 is any 2xx", path: "/204", wantCode: 204},
		{name: "500", path: "/500", wantKind: domain.ErrorUnexpectedStatus, wantCode: 500},
		{name: "expected status matches", path: "/500", expected: ptr(500), wantCode: 500},
		{name: "expected status differs", path: "/", expected: ptr(201), wantKind: domain.ErrorUnexpectedStatus, wantCode: 200},
		{name: "keyword found", path: "/", keyword: ptr("Domain"), wantCode: 200},
		{name: "keyword missing", path: "/", keyword: ptr("Login"), wantKind: domain.ErrorKeywordMismatch, wantCode: 200},
		{name: "keyword past the body limit", path: "/big", keyword: ptr("needle"), wantKind: domain.ErrorKeywordMismatch, wantCode: 200},
		{name: "HEAD", path: "/head", method: http.MethodHead, wantCode: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tg := target(srv.URL + tt.path)
			tg.ExpectedStatus, tg.Keyword = tt.expected, tt.keyword
			if tt.method != "" {
				tg.Method = tt.method
			}

			res := c.Check(t.Context(), tg)

			assert.Equal(t, tt.wantKind, res.ErrorKind, res.ErrorMessage)
			assert.Equal(t, tt.wantKind == "", res.Success)
			require.NotNil(t, res.StatusCode)
			assert.Equal(t, tt.wantCode, *res.StatusCode)
		})
	}
}

func TestCheckRedirects(t *testing.T) {
	t.Parallel()
	// /hop/N redirects to /hop/N-1; /hop/0 answers 200.
	srv := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n int
		_, _ = fmt.Sscanf(r.URL.Path, "/hop/%d", &n)
		if n == 0 {
			_, _ = io.WriteString(w, "arrived")
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
	}))
	c := New(Options{AllowPrivate: true})

	tests := []struct {
		name     string
		hops     int
		follow   bool
		wantKind domain.ErrorKind
		wantCode *int
	}{
		{name: "followed", hops: 3, follow: true, wantCode: ptr(200)},
		{name: "exactly the maximum", hops: MaxRedirects, follow: true, wantCode: ptr(200)},
		{name: "too many", hops: MaxRedirects + 1, follow: true, wantKind: domain.ErrorUnexpectedStatus},
		{name: "not followed: the 302 is judged", hops: 1, follow: false, wantKind: domain.ErrorUnexpectedStatus, wantCode: ptr(302)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tg := target(fmt.Sprintf("%s/hop/%d", srv.URL, tt.hops))
			tg.FollowRedirects = tt.follow

			res := c.Check(t.Context(), tg)

			assert.Equal(t, tt.wantKind, res.ErrorKind, res.ErrorMessage)
			assert.Equal(t, tt.wantCode, res.StatusCode)
		})
	}
}

func TestCheckTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := newServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })
	tg := target(srv.URL)
	tg.Timeout = 200 * time.Millisecond

	res := New(Options{AllowPrivate: true}).Check(t.Context(), tg)

	assert.Equal(t, domain.ErrorTimeout, res.ErrorKind, res.ErrorMessage)
	assert.False(t, res.Success)
	assert.Nil(t, res.StatusCode)
	assert.NotNil(t, res.ConnectMs, "the connection was made")
	assert.Nil(t, res.TTFBMs, "no answer came")
	assert.GreaterOrEqual(t, res.ResponseTimeMs, 150)
}

func TestCheckConnectionRefused(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	// Refused loopback connections take ~2 s on Windows; the timeout is
	// longer, so the refusal is what ends the check.
	res := New(Options{AllowPrivate: true}).Check(t.Context(), target("http://"+addr))

	assert.Equal(t, domain.ErrorConnection, res.ErrorKind, res.ErrorMessage)
	assert.Nil(t, res.ConnectMs)
}

func TestCheckDNSFailure(t *testing.T) {
	t.Parallel()
	c := New(Options{Resolver: fakeResolver{}})

	res := c.Check(t.Context(), target("https://does-not-exist.example"))

	assert.Equal(t, domain.ErrorDNS, res.ErrorKind)
	assert.Contains(t, res.ErrorMessage, "no such host")
	assert.NotNil(t, res.DNSMs, "the lookup itself happened")
}

func TestCheckBlocksPrivateTargets(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := newServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	c := New(Options{
		AllowPrivate: false,
		Resolver: fakeResolver{
			// DNS rebinding: a public-looking name that resolves to the
			// loopback interface at check time.
			"rebind.example": loopback,
			"mapped.example": {netip.MustParseAddr("::ffff:127.0.0.1")},
			"mixed.example":  {netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("127.0.0.1")},
		},
	})

	for _, host := range []string{"127.0.0.1", "rebind.example", "mapped.example", "mixed.example"} {
		t.Run(host, func(t *testing.T) {
			res := c.Check(t.Context(), target(withHost(t, srv.URL, host)))

			assert.Equal(t, domain.ErrorBlockedTarget, res.ErrorKind, res.ErrorMessage)
			assert.Contains(t, res.ErrorMessage, "blocked")
		})
	}
	assert.Zero(t, hits.Load(), "no request reached the internal server")
}

func TestSafeDialer(t *testing.T) {
	t.Parallel()
	srv := newServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.Listener.Addr().String()

	_, err := NewSafeDialer(false).DialContext(t.Context(), "tcp", addr)
	require.Error(t, err)
	assert.True(t, IsBlockedConnection(err), err)

	conn, err := NewSafeDialer(true).DialContext(t.Context(), "tcp", addr)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	assert.False(t, IsBlockedConnection(errors.New("connection refused")))
}

func TestRedirectPolicyBlocksPrivateTargets(t *testing.T) {
	t.Parallel()
	policy := New(Options{}).redirectPolicy(true)
	via := []*http.Request{httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/", http.NoBody)}

	for _, to := range []string{"http://169.254.169.254/latest/meta-data/", "http://localhost/", "ftp://example.com/"} {
		next := httptest.NewRequestWithContext(t.Context(), http.MethodGet, to, http.NoBody)

		err := policy(next, via)

		require.Error(t, err, to)
	}
	err := policy(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://169.254.169.254/", http.NoBody), via)
	assert.Equal(t, domain.ErrorBlockedTarget, classify(&url.Error{Op: "Get", URL: "https://example.com/", Err: err}))
}

func TestCheckTLSTimings(t *testing.T) {
	t.Parallel()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "secure")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	// The test certificate is issued for example.com; resolve it to the
	// server, so the check goes through DNS, TCP, TLS and HTTP/2.
	c := New(Options{
		AllowPrivate: true,
		Resolver:     fakeResolver{"example.com": loopback},
		TLSConfig:    &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	})

	res := c.Check(t.Context(), target(withHost(t, srv.URL, "example.com")))

	require.True(t, res.Success, res.ErrorMessage)
	// The values can be 0 ms on a fast machine; what matters is that every
	// phase was measured.
	assert.NotNil(t, res.DNSMs)
	assert.NotNil(t, res.ConnectMs)
	assert.NotNil(t, res.TLSMs)
	assert.NotNil(t, res.TTFBMs)
	assert.Equal(t, "HTTP/2.0", res.HTTPProto)
	assert.Equal(t, "TLS1.3", res.TLSVersion)
	require.NotNil(t, res.TLSNotAfter)
	assert.True(t, srv.Certificate().NotAfter.Equal(*res.TLSNotAfter))
}

func TestCheckSelfSignedCertificate(t *testing.T) {
	t.Parallel()
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// No RootCAs: the httptest certificate is not trusted.
	res := New(Options{AllowPrivate: true}).Check(t.Context(), target(srv.URL))

	assert.Equal(t, domain.ErrorTLS, res.ErrorKind, res.ErrorMessage)
	assert.Nil(t, res.TLSMs, "the handshake failed")
}

func TestCheckExpiredCertificate(t *testing.T) {
	t.Parallel()
	cert, roots := expiredCertificate(t)
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	c := New(Options{
		AllowPrivate: true,
		Resolver:     fakeResolver{"example.com": loopback},
		TLSConfig:    &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	})

	res := c.Check(t.Context(), target(withHost(t, srv.URL, "example.com")))

	assert.Equal(t, domain.ErrorTLS, res.ErrorKind, res.ErrorMessage)
	assert.Contains(t, res.ErrorMessage, "expired")
}

// expiredCertificate returns a self-signed certificate for example.com that
// expired yesterday, and a pool that trusts it: the only problem left is the
// expiry.
func expiredCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "example.com"},
		DNSNames:              []string{"example.com"},
		NotBefore:             now.Add(-48 * time.Hour),
		NotAfter:              now.Add(-24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	parsed, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want domain.ErrorKind
	}{
		{name: "blocked dial", err: &net.OpError{Op: "dial", Err: blockedError{addr: "127.0.0.1:80"}}, want: domain.ErrorBlockedTarget},
		{name: "blocked redirect", err: fmt.Errorf("redirect: %w", ErrURLBlocked), want: domain.ErrorBlockedTarget},
		{name: "redirect loop", err: &url.Error{Err: errTooManyRedirects}, want: domain.ErrorUnexpectedStatus},
		{name: "unknown CA", err: x509.UnknownAuthorityError{}, want: domain.ErrorTLS},
		{name: "wrong host name", err: x509.HostnameError{Certificate: &x509.Certificate{}, Host: "a"}, want: domain.ErrorTLS},
		{name: "TLS alert", err: tls.AlertError(40), want: domain.ErrorTLS},
		{name: "DNS", err: &net.DNSError{Err: "no such host", IsNotFound: true}, want: domain.ErrorDNS},
		{name: "DNS timeout is DNS", err: &net.DNSError{Err: "timeout", IsTimeout: true}, want: domain.ErrorDNS},
		{name: "deadline", err: &url.Error{Err: context.DeadlineExceeded}, want: domain.ErrorTimeout},
		{name: "refused", err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}, want: domain.ErrorConnection},
		{name: "closed mid-response", err: io.ErrUnexpectedEOF, want: domain.ErrorConnection},
		{name: "anything else", err: errors.New("weird"), want: domain.ErrorUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, classify(tt.err))
		})
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "short", truncate("short", 10))
	assert.Equal(t, "abc...", truncate("abcdef", 3))
	assert.Equal(t, "я...", truncate("яя", 3), "never cuts a rune in half")
}
