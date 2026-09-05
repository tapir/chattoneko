// Package webimage fetches images from arbitrary web URLs while looking
// like a real browser: identical TLS (JA3/JA4) and HTTP/2 fingerprints to a
// current Chrome, matching User-Agent and browser-typical request headers.
// Bot protection (Cloudflare, DataDome, hotlink guards, ...) blocks the
// stdlib's Go TLS fingerprint on sight; tls-client's browser profiles make
// our GET indistinguishable from a Chrome image request. Everything stays
// in memory — fetched bytes never touch the disk.
//
// SSRF defense: the URLs come from the model (ultimately from chat input),
// so every connection is vetted against a private/reserved-address blocklist
// in two layers — once before the request (clean errors) and again in the
// dial hook (the address actually connected to, covering redirect hops and
// shrinking the DNS-rebinding window). Redirects are validated hop by hop;
// HTTP/3 is disabled so nothing bypasses the dial hook.
package webimage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// ErrTooLarge is returned when the response body exceeds the requested cap.
var ErrTooLarge = errors.New("image too large")

// fetchTimeout bounds a single fetch. The tools layer additionally wraps
// every call in its own 30s cap.
const fetchTimeout = 25

// maxRedirects caps the redirect hops followed (browser-like, same as the
// stdlib client's limit).
const maxRedirects = 10

// Profile + headers must stay in sync: anti-bot systems cross-check the
// TLS fingerprint against the declared User-Agent, and a mismatch is an
// instant block. Chrome_150 is the freshest profile shipped by tls-client
// v1.16.x (Chrome profiles are the most frequently updated); bump the
// profile AND both version strings below together on library upgrades.
const (
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
	secChUA   = `"Google Chrome";v="150", "Chromium";v="150", "Not=A?Brand";v="24"`
)

var (
	clientOnce sync.Once
	client     tls_client.HttpClient
	clientErr  error
)

// httpClient returns the process-wide TLS client, built once. A shared
// client keeps the cookie jar and TLS session cache across fetches, which
// is what real browsers do (and what some anti-bot clearance flows expect).
func httpClient() (tls_client.HttpClient, error) {
	clientOnce.Do(func() {
		client, clientErr = tls_client.NewHttpClient(tls_client.NewNoopLogger(),
			tls_client.WithClientProfile(profiles.Chrome_150),
			tls_client.WithTimeoutSeconds(fetchTimeout),
			tls_client.WithCookieJar(tls_client.NewCookieJar()),
			// Chrome randomizes TLS extension order; the profile adds
			// GREASE values.
			tls_client.WithRandomTLSExtensionOrder(),
			tls_client.WithCatchPanics(),
			// SSRF defense: vet every connection and every redirect hop.
			// HTTP/3 is disabled so nothing can bypass the dial hook.
			tls_client.WithDialContext(ssrfDialContext),
			tls_client.WithCustomRedirectFunc(ssrfRedirectFunc),
			tls_client.WithDisableHttp3(),
		)
	})
	return client, clientErr
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

// ssrfDialContext guards every TCP connection (initial request and every
// redirect hop): the host's resolved addresses must all be public before
// the dial. The hostname itself is dialed (not a resolved IP) so TLS SNI
// keeps the name; the window between this check and the dialer's own
// resolution is microseconds on the same resolver cache.
func ssrfDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if err := checkHostPublic(ctx, host); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: time.Duration(fetchTimeout) * time.Second}
	return d.DialContext(ctx, network, addr)
}

// ssrfRedirectFunc vets every redirect hop before it is followed: an open
// redirector must not route the fetch to an internal address or a
// non-http(s) scheme.
func ssrfRedirectFunc(req *fhttp.Request, via []*fhttp.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("too many redirects (more than %d)", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
	}
	return checkHostPublic(req.Context(), req.URL.Hostname())
}

// requestHeaders builds the headers of a Chrome image request (the shape a
// browser sends when loading an <img> from another origin). Key order
// matters: HeaderOrderKey fixes the HTTP/1.1 on-the-wire order; keys must
// be lowercase. Accept-Encoding lists every codec we can decompress
// (gzip, deflate, brotli, zstd — see get). The Accept list deliberately
// omits image/avif and image/svg+xml even though real Chrome advertises
// them: content-negotiating CDNs (imgix / Unsplash's auto=format) honor
// avif by serving AVIF, which our conversion pipeline cannot decode, and
// SVG is text we cannot rasterize either.
func requestHeaders() fhttp.Header {
	h := fhttp.Header{
		"accept":             {"image/webp,image/apng,image/png,image/jpeg,image/*,*/*;q=0.8"},
		"accept-language":    {"en-US,en;q=0.9"},
		"sec-ch-ua":          {secChUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {`"Windows"`},
		"sec-fetch-dest":     {"image"},
		"sec-fetch-mode":     {"no-cors"},
		"sec-fetch-site":     {"cross-site"},
		"user-agent":         {userAgent},
		"accept-encoding":    {"gzip, deflate, br, zstd"},
	}
	h[fhttp.HeaderOrderKey] = []string{
		"accept", "accept-language", "sec-ch-ua", "sec-ch-ua-mobile",
		"sec-ch-ua-platform", "sec-fetch-dest", "sec-fetch-mode",
		"sec-fetch-site", "user-agent", "accept-encoding",
	}
	return h
}

// Fetch downloads rawURL (following redirects like a browser) and returns
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
	c, err := httpClient()
	if err != nil {
		return nil, nil, "", fmt.Errorf("http client unavailable: %w", err)
	}
	return get(ctx, c, u, maxBytes)
}

// get performs one GET. On HTTP 400 for a MediaWiki-style thumbnail URL it
// retries once against the original file (see originalOf); the retry result
// (success or error) wins.
func get(ctx context.Context, c tls_client.HttpClient, u *url.URL, maxBytes int64) ([]byte, *url.URL, string, error) {
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
// status check, transparent decompression, size cap. It also reports the
// HTTP status so callers can decide on fallbacks.
func getOnce(ctx context.Context, c tls_client.HttpClient, u *url.URL, maxBytes int64) ([]byte, *url.URL, string, int, error) {
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, u.String(), nil)
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
	// On HTTP/2 the transport does NOT transparently decompress bodies when
	// the caller set Accept-Encoding itself (only the HTTP/1.1 path does,
	// where it also deletes the header). A still-present Content-Encoding
	// therefore means a compressed body we must unpack ourselves.
	if ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); ce != "" && ce != "identity" {
		body = fhttp.DecompressBodyByType(body, ce)
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, nil, "", resp.StatusCode, fmt.Errorf("reading the image failed: %w", err)
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
