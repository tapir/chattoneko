// Package webfetch fetches arbitrary web URLs (images, JSON, HTML, any
// other body) while looking like a real browser: for https it uses a uTLS
// ClientHello whose JA3/JA4 fingerprint matches current Chrome (impersonate-http, whose profiles track
// utls's *_Auto templates) plus Chrome's own header values; for plain http
// there is no handshake to fingerprint, so a stock net/http client carries
// the same headers. Bot protection (Cloudflare, DataDome, hotlink guards,
// ...) blocks the stdlib's Go TLS fingerprint on sight. Everything stays in
// memory — fetched bytes never touch the disk.
//
// SSRF defense: the URLs come from the model (ultimately from chat input),
// so every connection is vetted against a private/reserved-address blocklist
// in two layers — once before the request (clean errors) and again in the
// dial function (the address actually connected to, covering redirect hops
// and shrinking the DNS-rebinding window). Redirects are validated hop by
// hop through the client's CheckRedirect.
//
// ponytail: the impersonating (https) client cannot be unit-tested —
// impersonate-http exposes no InsecureSkipVerify, so httptest's self-signed
// server is refused, and its transport always handshakes, so it cannot serve
// plain-http fakes either. The scheme-independent logic (SSRF vetting,
// headers, redirect hops, size cap, gzip, thumbnail fallback) is covered by
// the http:// tests; the fingerprint itself is verified in production.
package webfetch

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/North-web-dev/impersonate-http"
)

// ErrTooLarge is returned when the response body exceeds the requested cap.
var ErrTooLarge = errors.New("response too large")

// fetchTimeout bounds a single fetch. The tools layer additionally wraps
// every call in its own 30s cap.
const fetchTimeout = 25 * time.Second

// maxRedirects caps the redirect hops followed (browser-like, same as the
// stdlib client's limit).
const maxRedirects = 10

var (
	clientOnce sync.Once
	// tlsClient impersonates Chrome (https); plainClient is a stock client
	// for http:// URLs, where there is no TLS handshake to fingerprint.
	tlsClient, plainClient *http.Client
)

// newClients builds both process-wide clients once. A shared client per
// scheme keeps the cookie jar, the TLS session cache and the per-host
// transports across fetches, which is what real browsers do (and what some
// anti-bot clearance flows expect). Both are wired to the same SSRF dial
// function and redirect policy.
func newClients() (*http.Client, *http.Client) {
	clientOnce.Do(func() {
		tlsClient = impersonate.New(impersonate.Chrome,
			impersonate.WithDialer(ssrfDial),
			impersonate.WithTimeout(fetchTimeout),
		)
		plainClient = &http.Client{
			Timeout:   fetchTimeout,
			Transport: &http.Transport{DialContext: ssrfDial},
		}
		// Same jar for both: clearance cookies are per-host, not per-scheme.
		jar, err := cookiejar.New(nil)
		if err == nil {
			tlsClient.Jar, plainClient.Jar = jar, jar
		}
		tlsClient.CheckRedirect = ssrfCheckRedirect
		plainClient.CheckRedirect = ssrfCheckRedirect
	})
	return tlsClient, plainClient
}

// clientFor returns the client that should fetch u: the impersonating one
// for https, the stock one for plain http (impersonate-http always performs
// a TLS handshake, so it cannot serve an http:// URL).
func clientFor(u *url.URL) *http.Client {
	tls, plain := newClients()
	if u.Scheme == "https" {
		return tls
	}
	return plain
}

// testAllowLoopback relaxes the blocklist for loopback addresses so tests
// can reach their httptest servers on 127.0.0.1. Production never sets it.
var testAllowLoopback bool

// AllowLoopbackForTesting relaxes the SSRF blocklist for loopback addresses.
// TEST SUPPORT ONLY — fake servers in other packages' tests live on
// 127.0.0.1; production code must never call this.
func AllowLoopbackForTesting(on bool) { testAllowLoopback = on }

// reservedCIDRs are the ranges beyond net.IP's own predicates that browsers
// refuse to reach and we must never fetch from.
var reservedCIDRs = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8",     // "this" network
		"100.64.0.0/10", // CGNAT
		"192.0.0.0/24",  // IETF protocol assignments
		"198.18.0.0/15", // benchmarking
	} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// isBlockedIP reports whether an address is private/reserved and must never
// be fetched from (loopback, RFC1918, link-local incl. the cloud metadata
// 169.254.169.254, ULA, multicast, unspecified, CGNAT, ...).
func isBlockedIP(ip net.IP) bool {
	if testAllowLoopback && ip.IsLoopback() {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		for _, n := range reservedCIDRs {
			if n.Contains(v4) {
				return true
			}
		}
	}
	return false
}

// numericHost matches hosts made only of digits and dots — ambiguous IP
// spellings the system resolver may still interpret ("2130706433" connects
// to 127.0.0.1) while net.ParseIP rejects them. Such hosts are refused
// outright instead of trusting the resolver's interpretation.
var numericHost = regexp.MustCompile(`^[0-9.]+$`)

// errPrivateAddress is the SSRF verdict, phrased for the model/user.
var errPrivateAddress = errors.New("the URL points to a private or reserved network address, which cannot be fetched")

// checkHostPublic resolves host and refuses it when ANY of its addresses is
// private/reserved. It also refuses the ambiguous numeric spellings.
func checkHostPublic(ctx context.Context, host string) error {
	if numericHost.MatchString(host) && net.ParseIP(host) == nil {
		return errPrivateAddress
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("no addresses found for host %s", host)
	}
	for _, ipa := range ips {
		if isBlockedIP(ipa.IP) {
			return errPrivateAddress
		}
	}
	return nil
}

// ssrfDial guards every TCP connection (initial request and every redirect
// hop): the host's resolved addresses must all be public before the dial.
// The hostname itself is dialed (not a resolved IP) so TLS SNI keeps the
// name; the window between this check and the dialer's own resolution is
// microseconds on the same resolver cache. It serves both as the impersonate
// dialer and as the stock transport's DialContext.
func ssrfDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if err := checkHostPublic(ctx, host); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: fetchTimeout}
	return d.DialContext(ctx, network, addr)
}

// ssrfCheckRedirect vets every redirect hop before it is followed: an open
// redirector must not route the fetch to an internal address or a non-http(s)
// scheme.
func ssrfCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("too many redirects (more than %d)", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
	}
	return checkHostPublic(req.Context(), req.URL.Hostname())
}

// requestHeaders builds the headers of a Chrome navigation (the shape a
// browser sends when a URL is opened in a tab), which is what every kind of
// body — HTML, JSON, an image — is served to. The base is the library's
// Chrome profile, so the User-Agent, the sec-ch-ua version strings and the
// Sec-Fetch-* headers stay in sync with the fingerprinted ClientHello on
// library upgrades — no hardcoded version numbers here. Only Accept and the
// encoding are overridden.
//
// The Accept list deliberately omits image/avif and image/svg+xml even though
// real Chrome advertises them: content-negotiating CDNs (imgix / Unsplash's
// auto=format) honor avif by serving AVIF, which our image pipeline cannot
// decode. Everything else is covered by the trailing */*.
func requestHeaders() http.Header {
	h := impersonate.Chrome.Headers.Clone()
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,image/png,image/jpeg,*/*;q=0.8")
	// gzip only: neither transport decompresses a caller-declared encoding,
	// and br/zstd would need decoders we don't link (see getOnce).
	h.Set("Accept-Encoding", "gzip")
	return h
}

// Fetch GETs rawURL (following redirects like a browser) and returns
// the body (capped at maxBytes), the final URL after redirects, and the
// response Content-Type. Errors are phrased for the model/user.
func Fetch(ctx context.Context, rawURL string, maxBytes int64) ([]byte, *url.URL, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, nil, "", fmt.Errorf("%q is not a valid http(s) URL", rawURL)
	}
	// First SSRF layer up front for a clean error; the dial hook is the
	// authoritative second layer (it also covers redirect hops).
	if err := checkHostPublic(ctx, u.Hostname()); err != nil {
		return nil, nil, "", err
	}
	return get(ctx, clientFor(u), u, maxBytes)
}

// get performs one GET. On HTTP 400 for a MediaWiki-style thumbnail URL it
// retries once against the original file (see originalOf); the retry result
// (success or error) wins.
func get(ctx context.Context, c *http.Client, u *url.URL, maxBytes int64) ([]byte, *url.URL, string, error) {
	data, final, ctype, status, err := getOnce(ctx, c, u, maxBytes)
	if err == nil {
		return data, final, ctype, nil
	}
	if status == http.StatusBadRequest {
		if orig := originalOf(u); orig != nil {
			d, f, ct, _, e := getOnce(ctx, c, orig, maxBytes)
			return d, f, ct, e
		}
	}
	return nil, nil, "", err
}

// getOnce is the raw single request: browser headers, redirect following,
// status check, gzip decompression, size cap. It also reports the HTTP status
// so callers can decide on fallbacks.
func getOnce(ctx context.Context, c *http.Client, u *url.URL, maxBytes int64) ([]byte, *url.URL, string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, "", 0, err
	}
	req.Header = requestHeaders()
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, "", 0, fmt.Errorf("fetching %s failed: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, "", resp.StatusCode, fmt.Errorf("the server refused the request (HTTP %s)", resp.Status)
	}
	body := resp.Body
	// We declare Accept-Encoding ourselves, so neither transport unpacks the
	// body for us; gzip is the only encoding we advertise (see requestHeaders).
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get("Content-Encoding")), "gzip") {
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, nil, "", resp.StatusCode, fmt.Errorf("the gzip-compressed response could not be unpacked: %w", err)
		}
		defer zr.Close()
		body = zr
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, nil, "", resp.StatusCode, fmt.Errorf("reading the response failed: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, nil, "", resp.StatusCode, ErrTooLarge
	}
	return data, resp.Request.URL, resp.Header.Get("Content-Type"), resp.StatusCode, nil
}

// thumbWidth matches the "<N>px-" prefix of a MediaWiki thumbnail's
// trailing path segment (/.../thumb/<h1>/<h2>/<Name>.<ext>/<N>px-<Name>.<ext>).
var thumbWidth = regexp.MustCompile(`^\d+px-`)

// originalOf maps a MediaWiki-style thumbnail URL to its original file URL
// (drops the "thumb" path segment and the trailing "<N>px-..." segment);
// nil when the URL is not such a thumbnail. Works on the escaped path so
// percent-encoding of the original URL is preserved on the wire.
func originalOf(u *url.URL) *url.URL {
	segs := strings.Split(u.EscapedPath(), "/")
	if len(segs) < 3 || !thumbWidth.MatchString(segs[len(segs)-1]) {
		return nil
	}
	idx := -1
	for i, s := range segs {
		if s == "thumb" {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return nil
	}
	joined := strings.Join(append(append([]string{}, segs[:idx]...), segs[idx+1:len(segs)-1]...), "/")
	dec, err := url.PathUnescape(joined)
	if err != nil {
		dec = joined
		joined = ""
	}
	orig := *u
	orig.Path = dec
	orig.RawPath = joined
	orig.RawQuery = ""
	orig.Fragment = ""
	return &orig
}
