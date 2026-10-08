// Package webfetch retrieves a page and returns its body.
//
// A URL is chosen by the model, so the fetch has to answer two questions the
// model cannot be trusted with: where the request actually goes, and what comes
// back.
//
// The first is an address problem. A name resolves to an address, and a name
// that resolved publicly can resolve privately a moment later - so the
// addresses are verified once, pinned, and dialled directly. Redirects are
// re-checked, and a redirect that changes origin is refused outright rather
// than followed, because following it is how a public page hands the request to
// an internal one.
//
// The second is a trust problem, and this package does not solve it. A fetched
// page is text written by someone else. Nothing here decides which of its
// sentences are safe to believe. What this package guarantees is narrower and
// checkable: the bytes came from a public address, they are bounded, and they
// are returned as a body of a known kind. Presenting them as data rather than
// instruction is the caller's job.
package webfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Errors, distinguished so a caller can branch rather than parse a message.
//
// The set matches the vocabulary a web-fetch capability is expected to expose:
// a bad URL, a blocked address, a blocked redirect, an oversized body, a
// timeout, and a content type that cannot be represented.
var (
	ErrInvalidURL      = errors.New("webfetch: invalid url")
	ErrBlocked         = errors.New("webfetch: blocked address")
	ErrRedirectBlocked = errors.New("webfetch: blocked redirect")
	ErrTooLarge        = errors.New("webfetch: response too large")
	ErrTimeout         = errors.New("webfetch: timed out")
	ErrUnsupported     = errors.New("webfetch: unsupported content type")
)

const (
	// MaxBytes bounds the decoded body.
	MaxBytes = 256 * 1024

	// MaxChars bounds the rendered text, which is what the model reads and what
	// is billed again as input tokens. A byte cap alone does not bound it: one
	// byte can be one character, or a quarter of one.
	MaxChars = 60_000

	// Timeout bounds the whole exchange, redirects included.
	Timeout = 20 * time.Second

	// MaxRedirects bounds a chain.
	MaxRedirects = 5

	// MaxURLBytes bounds the URL itself.
	MaxURLBytes = 2048
)

// Kind is what a fetched body is.
//
// A closed set rather than a media type string. A caller that switches on this
// has to handle both arms, and a third kind would be a deliberate change across
// every consumer rather than a value appearing at runtime.
type Kind string

const (
	KindHTML Kind = "html"
	KindText Kind = "text"
)

// Page is a fetched resource.
type Page struct {
	// URL is the final URL, after redirects.
	URL string

	// StatusCode is the response's status, which is part of the fetched
	// resource's state rather than a failure. A 404 is a page.
	StatusCode int

	// Kind is what the body is.
	Kind Kind

	// Body is the decoded content.
	Body string

	// Truncated reports that the body was longer than the caps.
	Truncated bool
}

// Text returns the page's readable text.
//
// Rendered here rather than at fetch time, so a caller that wants the markup
// can have it: the seam returns what was retrieved, and presentation is a
// separate concern - one that changes without changing the transport.
func (p Page) Text() string {
	if p.Kind == KindHTML {
		return collapse(stripMarkup(p.Body))
	}

	return collapse(p.Body)
}

// Title returns the document title, when the page has one.
func (p Page) Title() string {
	if p.Kind != KindHTML {
		return ""
	}

	return extractTag(p.Body, "title")
}

// Fetch retrieves a URL through the plain HTTP provider.
func Fetch(ctx context.Context, rawURL string) (Page, error) {
	parsed, err := ParseURL(rawURL)
	if err != nil {
		return Page{}, err
	}

	// Resolved once, before the request, and pinned. Every connection that
	// follows uses this set and nothing else, so a name cannot resolve
	// differently between the check and the connection.
	pinned, err := resolve(ctx, parsed.Hostname())
	if err != nil {
		return Page{}, err
	}

	origin := originOf(parsed)

	client := &http.Client{
		Timeout:   Timeout,
		Transport: transport(pinned),
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= MaxRedirects {
				return fmt.Errorf("%w: too many redirects", ErrRedirectBlocked)
			}

			// A redirect to another origin is refused rather than followed.
			//
			// Following it would mean resolving and pinning a second host
			// mid-request, on the say-so of the first. The model can ask for the
			// second URL itself, and that is a new call with a new check.
			if originOf(next.URL) != origin {
				return fmt.Errorf("%w: the page redirected to another origin (%s)",
					ErrRedirectBlocked, originOf(next.URL))
			}

			return nil
		},
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return Page{}, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}

	request.Header.Set("Accept", "text/html,text/plain,application/json,application/xml,text/*;q=0.9,*/*;q=0.1")
	request.Header.Set("User-Agent", "maxlabs-cli/1.0 (+local agent)")

	response, err := client.Do(request)
	if err != nil {
		return Page{}, classify(err)
	}

	defer func() { _ = response.Body.Close() }()

	kind, err := kindOf(response.Header.Get("Content-Type"))
	if err != nil {
		return Page{}, err
	}

	// One byte past the cap, so a body of exactly MaxBytes is not called
	// truncated.
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxBytes+1))
	if err != nil {
		return Page{}, classify(err)
	}

	truncated := len(body) > MaxBytes
	if truncated {
		body = body[:MaxBytes]
	}

	final := parsed.String()
	if response.Request != nil && response.Request.URL != nil {
		final = response.Request.URL.String()
	}

	// A non-2xx response is returned, not raised. The status is part of what was
	// retrieved: a 404 has a body that says why, and a model can act on it where
	// it can only retry against an error.
	return Page{
		URL:        final,
		StatusCode: response.StatusCode,
		Kind:       kind,
		Body:       string(body),
		Truncated:  truncated,
	}, nil
}

// ParseURL accepts only what this tool is willing to reach.
func ParseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)

	if raw == "" {
		return nil, fmt.Errorf("%w: no url", ErrInvalidURL)
	}

	if len(raw) > MaxURLBytes {
		return nil, fmt.Errorf("%w: the url is too long", ErrInvalidURL)
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: %s is not a web address", ErrInvalidURL, parsed.Scheme)
	}

	if parsed.Host == "" {
		return nil, fmt.Errorf("%w: no host", ErrInvalidURL)
	}

	// Credentials in a URL reach a service that expects them, and nothing here
	// needs to.
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: the url carries credentials", ErrInvalidURL)
	}

	// A literal address is checked here; a name is checked when it resolves.
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && !isPublic(ip) {
		return nil, fmt.Errorf("%w: %s is not a public address", ErrBlocked, parsed.Hostname())
	}

	if port := parsed.Port(); port != "" && port != "80" && port != "443" {
		return nil, fmt.Errorf("%w: port %s is not a web port", ErrBlocked, port)
	}

	return parsed, nil
}

// resolve verifies every address a name resolves to and returns the set to pin.
//
// All of them, not the first: a name with one public and one private address
// would otherwise be reachable half the time, depending on which the resolver
// listed first.
func resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !isPublic(ip) {
			return nil, fmt.Errorf("%w: %s is not a public address", ErrBlocked, host)
		}

		return []net.IP{ip}, nil
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: %s could not be resolved: %v", ErrBlocked, host, err)
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: %s resolved to nothing", ErrBlocked, host)
	}

	pinned := make([]net.IP, 0, len(addrs))

	for _, addr := range addrs {
		if !isPublic(addr.IP) {
			return nil, fmt.Errorf("%w: %s resolves to a private address (%s)",
				ErrBlocked, host, addr.IP)
		}

		pinned = append(pinned, addr.IP)
	}

	return pinned, nil
}

// transport dials only the pinned addresses.
//
// The connection is made to an address that was verified a moment ago, not to
// whatever the name resolves to now. That is the difference between checking a
// name and checking the thing actually connected to.
func transport(pinned []net.IP) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("%w: %s", ErrBlocked, address)
			}

			var lastErr error

			for _, ip := range pinned {
				dialer := &net.Dialer{Timeout: 10 * time.Second}

				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}

				lastErr = err
			}

			return nil, fmt.Errorf("%w: no pinned address answered: %v", ErrBlocked, lastErr)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		DisableKeepAlives:     true,
	}
}

// kindOf classifies a content type into the closed set of bodies.
func kindOf(contentType string) (Kind, error) {
	value := strings.ToLower(firstToken(contentType))

	if value == "" {
		// A server that declares nothing is read as text, which is what a bare
		// HTTP server serving a file does.
		return KindText, nil
	}

	if strings.Contains(value, "html") || strings.HasSuffix(value, "+xml") && strings.Contains(value, "xhtml") {
		return KindHTML, nil
	}

	if strings.HasPrefix(value, "text/") {
		return KindText, nil
	}

	switch value {
	case "application/json", "application/xml", "application/rss+xml", "application/atom+xml":
		return KindText, nil
	}

	if strings.HasSuffix(value, "+json") {
		return KindText, nil
	}

	return "", fmt.Errorf("%w: %q", ErrUnsupported, value)
}

// classify turns a transport failure into one of the package's errors.
//
// A caller branches on these rather than reading them, so a message is not the
// interface.
func classify(err error) error {
	switch {
	case errors.Is(err, ErrBlocked), errors.Is(err, ErrRedirectBlocked):
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Timeout() {
			return fmt.Errorf("%w: %v", ErrTimeout, urlErr)
		}

		return classify(urlErr.Err)
	}

	return fmt.Errorf("webfetch: %w", err)
}

// originOf is the scheme and authority a redirect must preserve.
func originOf(u *url.URL) string {
	if u == nil {
		return ""
	}

	return u.Scheme + "://" + u.Host
}

// firstToken returns a media type without its parameters.
func firstToken(contentType string) string {
	if idx := strings.Index(contentType, ";"); idx >= 0 {
		return strings.TrimSpace(contentType[:idx])
	}

	return strings.TrimSpace(contentType)
}
