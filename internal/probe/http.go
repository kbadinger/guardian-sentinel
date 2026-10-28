package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// maxBodyBytes caps how much of a page is read when looking for Expect.
const maxBodyBytes = 1 << 20 // 1 MiB

// HTTP checks that a URL answers 200 with a body containing Expect.
// Redirects are followed. For HTTPS it also warns when the certificate
// expires within WarnCertDays (0 turns the warning off).
type HTTP struct {
	URL          string
	Expect       string
	WarnCertDays int

	Client *http.Client     // nil means NewHTTPClient()
	Now    func() time.Time // nil means time.Now; tests set a fixed clock
}

// NewHTTPClient returns the client the http probe uses by default. Keep-alives
// are off so every check makes a fresh DNS lookup, TCP connection and TLS
// handshake, and sees a renewed certificate as soon as it is served. The
// timeout comes from the context, not the client.
func NewHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	return &http.Client{Transport: t}
}

// Check runs one request.
func (h *HTTP) Check(ctx context.Context) Result {
	client := h.Client
	if client == nil {
		client = NewHTTPClient()
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return Fail("bad request: %v", err)
	}
	req.Header.Set("User-Agent", "guardian-sentinel")

	resp, err := client.Do(req)
	if err != nil {
		return Fail("%s", requestError(err))
	}
	defer resp.Body.Close()

	var r Result
	// resp.TLS describes the connection of the final response, after redirects.
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		r.Warn = CertWarning(resp.Request.URL.Hostname(), resp.TLS.PeerCertificates[0].NotAfter, now(), h.WarnCertDays)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	switch {
	case resp.StatusCode != http.StatusOK:
		r.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
	case err != nil:
		r.Err = fmt.Sprintf("reading body: %s", requestError(err))
	case !strings.Contains(string(body), h.Expect):
		r.Err = fmt.Sprintf("page does not contain %q", h.Expect)
	default:
		r.OK = true
	}
	return r
}

// CertWarning returns a warning when notAfter is within warnDays of now, or
// "" when it is not (or warnDays is 0). An already-expired certificate fails
// the TLS handshake, so the check fails before it gets here.
func CertWarning(host string, notAfter, now time.Time, warnDays int) string {
	if warnDays <= 0 {
		return ""
	}
	left := notAfter.Sub(now)
	if left > time.Duration(warnDays)*24*time.Hour {
		return ""
	}
	switch days := int(left / (24 * time.Hour)); days {
	case 0:
		return fmt.Sprintf("cert for %s expires in less than a day", host)
	case 1:
		return fmt.Sprintf("cert for %s expires in 1 day", host)
	default:
		return fmt.Sprintf("cert for %s expires in %d days", host, days)
	}
}

// requestError turns a client error into a short reason for an alert. It
// drops the *url.Error wrapper, which repeats the whole URL.
func requestError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}
