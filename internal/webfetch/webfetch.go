// Package webfetch fetches arbitrary web URLs (images, JSON, HTML, any other
// body) while looking like a real browser: for https it uses
// bogdanfinn/tls-client, whose uTLS ClientHello and HTTP/2 framing carry the
// JA3/JA4 fingerprint of the Chrome profile it impersonates; for plain http
// there is no handshake to fingerprint, so a stock client carries the same
// headers. Bot protection (Cloudflare, DataDome, hotlink guards, ...) blocks
// the stdlib's Go TLS fingerprint on sight. Everything stays in memory —
// fetched bytes never touch the disk.
//
// Both clients speak bogdanfinn/fhttp, the net/http fork tls-client is built
// on. It is not interchangeable with the stdlib here: it is the only one of
// the two that can put request headers on the wire in a chosen order, and
// header order is part of what gets fingerprinted.
//
// SSRF defense: the URLs come from the model (ultimately from chat input), so
// every connection is vetted against a private/reserved-address blocklist in
// two layers — once before the request (clean errors) and again in the dial
// function (the address actually connected to, covering redirect hops and
// shrinking the DNS-rebinding window). Redirects are validated hop by hop
// through the client's redirect policy.
package webfetch

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/cookiejar"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// ErrTooLarge is returned when the response body exceeds the requested cap.
var ErrTooLarge = errors.New("response too large")

// fetchTimeout bounds a single fetch. The fetch tool that calls this declares
// its own 2-minute cap on top, because a stored body can go through ffmpeg.
const fetchTimeout = 25 * time.Second

// maxRedirects caps the redirect hops followed (browser-like, same as the
// stdlib client's limit).
const maxRedirects = 10

var (
	clientOnce sync.Once
	// tlsClient impersonates Chrome (https); plainClient is a stock client
	// for http:// URLs, where there is no TLS handshake to fingerprint.
	tlsClient   tlsclient.HttpClient
	plainClient *http.Client
	clientErr   error
)

// doer is the request entry point both clients share.
type doer interface {
	Do(*http.Request) (*http.Response, error)
}

// newClients builds both process-wide clients once. A shared client per
// scheme keeps the cookie jar, the TLS session cache and the per-host
// transports across fetches, which is what real browsers do (and what some
// anti-bot clearance flows expect). Both are wired to the same SSRF dial
// function and redirect policy.
func newClients() (doer, doer, error) {
	clientOnce.Do(func() {
		// Same jar for both: clearance cookies are per-host, not per-scheme.
		jar, err := cookiejar.New(nil)
		if err != nil {
			clientErr = err
			return
		}
		opts := []tlsclient.HttpClientOption{
			tlsclient.WithClientProfile(profiles.Chrome_146),
			tlsclient.WithTimeoutMilliseconds(int(fetchTimeout / time.Millisecond)),
			tlsclient.WithDialContext(ssrfDial),
			tlsclient.WithCustomRedirectFunc(ssrfCheckRedirect),
			tlsclient.WithCookieJar(jar),
			// Chrome only offers h3 in ALPN once Alt-Svc told it to, and QUIC
			// dials over UDP, which ssrfDial never sees.
			tlsclient.WithDisableHttp3(),
		}
		if testInsecureTLS {
			opts = append(opts, tlsclient.WithInsecureSkipVerify())
		}
		tlsClient, clientErr = tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), opts...)
		if clientErr != nil {
			return
		}
		plainClient = &http.Client{
			Jar:           jar,
			Timeout:       fetchTimeout,
			Transport:     &http.Transport{DialContext: ssrfDial},
			CheckRedirect: ssrfCheckRedirect,
		}
	})
	return tlsClient, plainClient, clientErr
}

// clientFor returns the client that should fetch u: the impersonating one for
// https, the stock one for plain http. They cannot be one client even though
// tls-client serves both schemes: it keys its per-host transport cache on
// host:443 whatever the scheme, so a plain-http fetch would poison the https
// entry and vice versa.
func clientFor(u *url.URL) (doer, error) {
	tls, plain, err := newClients()
	if err != nil {
		return nil, fmt.Errorf("building the fetch client failed: %w", err)
	}
	if u.Scheme == "https" {
		return tls, nil
	}
	return plain, nil
}

// testAllowLoopback relaxes the blocklist for loopback addresses and
// testInsecureTLS accepts httptest's self-signed certificate. Both are read
// once, when the clients are built, so flipping them means rebuilding.
var (
	testAllowLoopback bool
	testInsecureTLS   bool
)

// AllowLoopbackForTesting lets tests reach their httptest servers on
// 127.0.0.1, over plain http and over https with the self-signed test
// certificate. TEST SUPPORT ONLY — production code must never call this.
func AllowLoopbackForTesting(on bool) {
	testAllowLoopback, testInsecureTLS = on, on
	clientOnce = sync.Once{}
}

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
	// ponytail: one retry on a transient DNS failure — systemd-resolved
	// SERVFAILs the odd query on an upstream blip and browsers retry
	// silently; NXDOMAIN is definitive, so it gets no second attempt. The
	// dial-time lookup right after rides this answer's resolver cache.
	if err != nil && dnsRetryable(err) {
		ips, err = net.DefaultResolver.LookupIPAddr(ctx, host)
	}
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
// microseconds on the same resolver cache. It serves both as tls-client's
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
// body — HTML, JSON, an image — is served to. A tls-client profile describes
// the handshake and the HTTP/2 framing only, so these values are pinned to
// the same Chrome milestone as the profile in newClients by hand: a
// User-Agent or brand list that disagrees with the fingerprinted ClientHello
// is itself a detection signal.
//
// The Accept list deliberately omits image/avif and image/svg+xml even though
// real Chrome advertises them: content-negotiating CDNs (imgix / Unsplash's
// auto=format) honor avif by serving AVIF, which our image pipeline cannot
// decode. Everything else is covered by the trailing */*.
func requestHeaders() http.Header {
	return http.Header{
		// Chrome's own navigation order. fhttp writes listed headers in this
		// order and appends the rest; the jar's Cookie is listed so it lands
		// where Chrome puts it instead of last.
		http.HeaderOrderKey: {
			"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
			"upgrade-insecure-requests", "user-agent", "accept",
			"sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest",
			"accept-encoding", "accept-language", "cookie", "priority",
		},
		// Keys are assigned in lowercase rather than through Header.Set, which
		// canonicalizes: Chrome puts the client hints on the wire in lowercase
		// and fhttp writes map keys verbatim.
		"sec-ch-ua":                 {`"Chromium";v="146", "Not-A.Brand";v="24", "Google Chrome";v="146"`},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {`"Windows"`},
		"upgrade-insecure-requests": {"1"},
		"user-agent":                {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"},
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,image/png,image/jpeg,*/*;q=0.8"},
		"sec-fetch-site":            {"none"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-user":            {"?1"},
		"sec-fetch-dest":            {"document"},
		// gzip only: br/zstd would need decoders we don't link; getOnce
		// unpacks gzip itself, by magic bytes.
		"accept-encoding": {"gzip"},
		"accept-language": {"en-US,en;q=0.9"},
		"priority":        {"u=0, i"},
	}
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
	c, err := clientFor(u)
	if err != nil {
		return nil, nil, "", err
	}
	return get(ctx, c, u, maxBytes)
}

// get performs one GET. On HTTP 400 for a MediaWiki-style thumbnail URL it
// retries once against the original file (see originalOf); the retry result
// (success or error) wins.
func get(ctx context.Context, c doer, u *url.URL, maxBytes int64) ([]byte, *url.URL, string, error) {
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

// dnsRetryable reports whether err is a DNS failure worth one more attempt
// (SERVFAIL, timeout, refused); a missing name is not.
func dnsRetryable(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && !de.IsNotFound
}

// getOnce is the raw single request: browser headers, redirect following,
// status check, gzip decompression, size cap. It also reports the HTTP status
// so callers can decide on fallbacks.
func getOnce(ctx context.Context, c doer, u *url.URL, maxBytes int64) ([]byte, *url.URL, string, int, error) {
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
	// Unpack by gzip magic bytes, not Content-Encoding: tls-client already
	// unpacks gzip on some paths (HTTP/2) yet leaves the header set, so a
	// header-only check gunzips an already-plain body and fails on it.
	br := bufio.NewReader(resp.Body)
	var body io.Reader = br
	if m, _ := br.Peek(2); len(m) == 2 && m[0] == 0x1f && m[1] == 0x8b {
		zr, err := gzip.NewReader(br)
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
