package runtime

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/tendant/agentbox/pkg/agentbox/audit"
	abegress "github.com/tendant/agentbox/pkg/agentbox/egress"
)

// systemRoots are where hosts keep their root certificate bundle: Debian and
// Ubuntu, Fedora and RHEL, Alpine and macOS.
var systemRoots = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/cert.pem",
}

// inspectTrust makes a run's certificate authority for egress inspection and
// writes, into dir (the run's tmp, which the sandbox reads), its certificate
// and a bundle of the system roots with it. The variables point the
// sandbox's tools at them: those that take a whole bundle (Go, OpenSSL,
// curl, git, Python, cargo) at the bundle, so hosts that are not inspected
// still verify; Node (the agent CLIs, npm) adds the one certificate to its
// own roots.
func inspectTrust(dir string) (*abegress.Inspector, []string, error) {
	var roots []byte
	for _, p := range systemRoots {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			roots = b
			break
		}
	}
	if roots == nil {
		return nil, nil, errors.New("no system root certificate bundle to add the run's certificate to")
	}
	in, err := abegress.NewInspector()
	if err != nil {
		return nil, nil, err
	}
	ca := filepath.Join(dir, "agentbox-egress-ca.pem")
	bundle := filepath.Join(dir, "agentbox-egress-roots.pem")
	if err := os.WriteFile(ca, in.CertPEM(), 0o644); err != nil {
		return nil, nil, err
	}
	b := append(append(append([]byte{}, roots...), '\n'), in.CertPEM()...)
	if err := os.WriteFile(bundle, b, 0o644); err != nil {
		return nil, nil, err
	}
	return in, []string{
		"SSL_CERT_FILE=" + bundle,
		"CURL_CA_BUNDLE=" + bundle,
		"GIT_SSL_CAINFO=" + bundle,
		"REQUESTS_CA_BUNDLE=" + bundle,
		"PIP_CERT=" + bundle,
		"CARGO_HTTP_CAINFO=" + bundle,
		"NODE_EXTRA_CA_CERTS=" + ca,
	}, nil
}

// auditHTTP records one request the egress gateway inspected.
func (r *Runtime) auditHTTP(runID string, i abegress.RequestInfo) {
	r.audit(runID, audit.TypeHTTPRequest, audit.HTTPRequestDetail{Host: i.Host, Method: i.Method, Path: i.Path, Status: i.Status,
		RequestBytes: i.RequestBytes, ResponseBytes: i.ResponseBytes, DurationMS: i.Duration.Milliseconds(), Protocol: i.Protocol, Error: i.Error,
		Credentials: i.Credentials})
}
