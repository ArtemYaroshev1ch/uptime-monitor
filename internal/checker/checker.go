package checker

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/artemyaroshev1ch/uptime-monitor/internal/domain"
	"github.com/artemyaroshev1ch/uptime-monitor/internal/platform/clock"
)

const (
	// MaxBodyBytes is how much of a response body is read (for the keyword
	// check). The rest is never downloaded.
	MaxBodyBytes = 1 << 20
	// MaxRedirects is how many redirects a check follows.
	MaxRedirects = 5
	// maxErrorMessage bounds the stored error text.
	maxErrorMessage = 500
)

// Resolver looks up the IP addresses of a host. *net.Resolver implements
// it; tests inject fakes, e.g. to simulate DNS rebinding.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Options configures a Checker.
type Options struct {
	// AllowPrivate (ALLOW_PRIVATE_TARGETS) permits internal addresses.
	AllowPrivate bool
	// Resolver defaults to net.DefaultResolver.
	Resolver Resolver
	// TLSConfig defaults to the system roots. Tests set RootCAs to trust
	// their httptest servers.
	TLSConfig *tls.Config
	// Clock defaults to the system clock.
	Clock clock.Clock
	// UserAgent is sent with every check, so site owners can identify (and
	// allow-list) the monitor.
	UserAgent string
}

// Checker performs HTTP/HTTPS checks. It is safe for concurrent use.
type Checker struct {
	policy    TargetPolicy
	resolver  Resolver
	clock     clock.Clock
	userAgent string
	transport *http.Transport
	dialer    *net.Dialer
}

// blockedError is returned when a connection to a blocked address is refused.
type blockedError struct{ addr string }

func (e blockedError) Error() string {
	return "connection to " + e.addr + " is blocked (non-public address)"
}

var errTooManyRedirects = fmt.Errorf("stopped after %d redirects", MaxRedirects)

// New returns a Checker.
func New(opts Options) *Checker {
	c := &Checker{
		policy:    NewTargetPolicy(opts.AllowPrivate),
		resolver:  opts.Resolver,
		clock:     opts.Clock,
		userAgent: opts.UserAgent,
	}
	if c.resolver == nil {
		c.resolver = net.DefaultResolver
	}
	if c.clock == nil {
		c.clock = clock.System{}
	}
	c.dialer = NewSafeDialer(opts.AllowPrivate)

	var tlsConfig *tls.Config
	if opts.TLSConfig != nil {
		tlsConfig = opts.TLSConfig.Clone()
	}
	c.transport = &http.Transport{
		// Never use HTTP_PROXY: the address checks would then apply to the
		// proxy instead of the target.
		Proxy:           nil,
		DialContext:     c.dial,
		TLSClientConfig: tlsConfig,
		// A fresh connection per check, so every check measures the full
		// DNS -> TCP -> TLS -> request cycle, as a new visitor would.
		DisableKeepAlives: true,
		// A custom DialContext turns HTTP/2 off unless forced.
		ForceAttemptHTTP2:      true,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return c
}

// NewSafeDialer returns a dialer that refuses to connect to non-public
// addresses (see IsBlockedIP) unless allowPrivate is set. Everything that
// connects to a user-supplied URL uses it: the checker and webhook delivery.
func NewSafeDialer(allowPrivate bool) *net.Dialer {
	d := &net.Dialer{KeepAlive: -1}
	if !allowPrivate {
		// Control runs after the address is known and before connecting:
		// the one place where every connection, including those made for
		// redirects, passes through with its final IP. Checking here
		// defeats DNS rebinding (a name that resolved to a public address
		// at validation time and to 127.0.0.1 now).
		d.Control = blockNonPublic
	}
	return d
}

// blockNonPublic is net.Dialer.Control: it rejects blocked addresses.
func blockNonPublic(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || IsBlockedIP(ap.Addr()) {
		return blockedError{addr: address}
	}
	return nil
}

// IsBlockedConnection reports whether err comes from NewSafeDialer refusing
// a non-public address.
func IsBlockedConnection(err error) bool { return isBlocked(err) }

// dial resolves the host itself (to time DNS with the injected resolver)
// and connects to the resolved addresses in turn.
func (c *Checker) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		tm := timingsFrom(ctx)
		start := c.clock.Now()
		ips, err = c.resolver.LookupNetIP(ctx, "ip", host)
		tm.setDNS(c.clock.Now().Sub(start))
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
		}
	}

	var errs []error
	for _, ip := range ips {
		conn, err := c.dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

// Check performs one check of target. It never returns an error: every
// failure is a result with an ErrorKind.
func (c *Checker) Check(ctx context.Context, target domain.CheckTarget) domain.CheckResult {
	start := c.clock.Now()
	res := domain.CheckResult{CheckedAt: start}

	ctx, cancel := context.WithTimeout(ctx, target.Timeout)
	defer cancel()
	tm := &timings{clock: c.clock}
	ctx = httptrace.WithClientTrace(context.WithValue(ctx, timingsKey{}, tm), tm.trace())

	req, err := http.NewRequestWithContext(ctx, target.Method, target.URL, http.NoBody)
	if err != nil {
		return c.fail(res, start, tm, domain.ErrorUnknown, err)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	client := &http.Client{Transport: c.transport, CheckRedirect: c.redirectPolicy(target.FollowRedirects)}
	resp, err := client.Do(req)
	if err != nil {
		return c.fail(res, start, tm, classify(err), err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body []byte
	if target.Method != http.MethodHead {
		// Reading the body (up to the limit) is part of the response time
		// and needed for the keyword check.
		body, err = io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes))
		if err != nil {
			res.StatusCode = &resp.StatusCode
			describeResponse(&res, resp)
			return c.fail(res, start, tm, classify(err), fmt.Errorf("read body: %w", err))
		}
	}

	res.StatusCode = &resp.StatusCode
	describeResponse(&res, resp)
	tm.fill(&res)
	res.ResponseTimeMs = millis(c.clock.Now().Sub(start))

	switch {
	case !statusOK(resp.StatusCode, target.ExpectedStatus):
		res.ErrorKind = domain.ErrorUnexpectedStatus
		res.ErrorMessage = fmt.Sprintf("status %d, expected %s", resp.StatusCode, expectedText(target.ExpectedStatus))
	case target.Keyword != nil && !bytes.Contains(body, []byte(*target.Keyword)):
		res.ErrorKind = domain.ErrorKeywordMismatch
		res.ErrorMessage = fmt.Sprintf("keyword not found in the first %d bytes of the body", MaxBodyBytes)
	default:
		res.Success = true
	}
	return res
}

func (c *Checker) fail(res domain.CheckResult, start time.Time, tm *timings, kind domain.ErrorKind, err error) domain.CheckResult {
	tm.fill(&res)
	res.ResponseTimeMs = millis(c.clock.Now().Sub(start))
	res.ErrorKind = kind
	res.ErrorMessage = truncate(err.Error(), maxErrorMessage)
	return res
}

// redirectPolicy follows at most MaxRedirects redirects, and only to URLs
// the target policy allows. The dialer checks the resolved address of each
// hop on top of that.
func (c *Checker) redirectPolicy(follow bool) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !follow {
			// Return the redirect response itself, to be judged by its status.
			return http.ErrUseLastResponse
		}
		if len(via) > MaxRedirects {
			return errTooManyRedirects
		}
		if err := c.policy.ValidateURL(req.URL.String()); err != nil {
			return fmt.Errorf("redirect to %s: %w", req.URL.Redacted(), err)
		}
		return nil
	}
}

func describeResponse(res *domain.CheckResult, resp *http.Response) {
	res.HTTPProto = resp.Proto
	if resp.TLS != nil {
		// tls.VersionName gives "TLS 1.3"; the stored form is "TLS1.3".
		res.TLSVersion = strings.ReplaceAll(tls.VersionName(resp.TLS.Version), " ", "")
		if len(resp.TLS.PeerCertificates) > 0 {
			notAfter := resp.TLS.PeerCertificates[0].NotAfter
			res.TLSNotAfter = &notAfter
		}
	}
}

func statusOK(code int, expected *int) bool {
	if expected == nil {
		return code >= 200 && code < 300
	}
	return code == *expected
}

func expectedText(expected *int) string {
	if expected == nil {
		return "2xx"
	}
	return fmt.Sprint(*expected)
}

func millis(d time.Duration) int {
	return int(d.Round(time.Millisecond) / time.Millisecond)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut at a rune boundary so the text stays valid UTF-8.
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
