package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPCheck(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("<html><title>Example Domain</title></html>"))
	})
	mux.HandleFunc("/wrong-app", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("<html><title>Some other app</title></html>"))
	})
	mux.HandleFunc("/error", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Example Domain is broken", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok", http.StatusFound)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tests := []struct {
		path    string
		timeout time.Duration
		wantOK  bool
		wantErr string
	}{
		{"/ok", time.Second, true, ""},
		{"/old", time.Second, true, ""}, // redirect is followed
		{"/wrong-app", time.Second, false, `page does not contain "Example Domain"`},
		{"/error", time.Second, false, "HTTP 503"}, // body has the text, but status is not 200
		{"/missing", time.Second, false, "HTTP 404"},
		{"/slow", 50 * time.Millisecond, false, "timed out"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			h := &HTTP{URL: srv.URL + tt.path, Expect: "Example Domain", WarnCertDays: 14, Client: srv.Client()}
			ctx, cancel := context.WithTimeout(context.Background(), tt.timeout)
			defer cancel()
			r := h.Check(ctx)
			if r.OK != tt.wantOK || r.Err != tt.wantErr {
				t.Errorf("got OK=%v Err=%q, want OK=%v Err=%q", r.OK, r.Err, tt.wantOK, tt.wantErr)
			}
			if r.Warn != "" {
				t.Errorf("plain HTTP should never warn about certs, got %q", r.Warn)
			}
		})
	}
}

func TestHTTPConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens on this local port any more

	h := &HTTP{URL: url, Expect: "x"}
	r := h.Check(context.Background())
	if r.OK || r.Err != "connection refused" {
		t.Errorf("got OK=%v Err=%q, want connection refused", r.OK, r.Err)
	}
}

func TestHTTPCertExpiryWarning(t *testing.T) {
	// A locally generated cert that expires in 5 days.
	cert := newTestCert(t, 5*24*time.Hour)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("Example Domain"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	tests := []struct {
		name     string
		warnDays int
		wantWarn string
	}{
		{"inside the window", 14, "cert for 127.0.0.1 expires in 4 days"}, // 5 days minus a few ms rounds down
		{"outside the window", 3, ""},
		{"warnings off", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &HTTP{URL: srv.URL, Expect: "Example Domain", WarnCertDays: tt.warnDays, Client: srv.Client()}
			r := h.Check(context.Background())
			if !r.OK {
				t.Fatalf("an expiring cert must not fail the check: %q", r.Err)
			}
			if r.Warn != tt.wantWarn {
				t.Errorf("warn = %q, want %q", r.Warn, tt.wantWarn)
			}
		})
	}
}

func TestCertWarning(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	tests := []struct {
		name     string
		left     time.Duration
		warnDays int
		want     string
	}{
		{"far away", 60 * day, 14, ""},
		{"exactly at the limit", 14 * day, 14, "cert for example.com expires in 14 days"},
		{"just outside", 14*day + time.Minute, 14, ""},
		{"nine days", 9*day + time.Hour, 14, "cert for example.com expires in 9 days"},
		{"one day", day + time.Hour, 14, "cert for example.com expires in 1 day"},
		{"hours left", 3 * time.Hour, 14, "cert for example.com expires in less than a day"},
		{"warnings off", day, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CertWarning("example.com", now.Add(tt.left), now, tt.warnDays); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{45 * time.Second, "45s"},
		{14*time.Minute + 59*time.Second, "14m"},
		{2 * time.Hour, "2h"},
		{2*time.Hour + 5*time.Minute, "2h5m"},
		{72 * time.Hour, "3d"},
		{76 * time.Hour, "3d4h"},
	}
	for _, tt := range tests {
		if got := FormatDuration(tt.in); got != tt.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// newTestCert makes a self-signed cert for 127.0.0.1 that expires after
// validFor. Nothing is written to disk.
func newTestCert(t *testing.T, validFor time.Duration) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sentinel test"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validFor),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
