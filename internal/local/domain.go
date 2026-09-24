package local

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- macOS Keychain identifies certificates by SHA-1; this is not a signature.
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type CloudOrigins struct {
	Cloud        string `json:"cloud"`
	Manufacturer string `json:"manufacturer"`
}

var (
	domainOrigins = CloudOrigins{
		Cloud:        "https://cloud.thermora.io",
		Manufacturer: "https://manufacturer.thermora.io",
	}
	localhostOrigins = CloudOrigins{
		Cloud:        "https://localhost:9443",
		Manufacturer: "https://manufacturer.localhost:9444",
	}
)

func ReadCloudOrigins() (CloudOrigins, error) {
	var origins CloudOrigins
	if err := Read(filepath.Join(CloudRoot, "public-origins.json"), &origins); errors.Is(err, os.ErrNotExist) {
		return domainOrigins, nil
	} else if err != nil {
		return origins, err
	}
	if origins != domainOrigins && origins != localhostOrigins {
		return origins, errors.New("invalid local cloud origins")
	}
	return origins, nil
}

// ConfigureCloudOrigins selects a local-only public entry point without changing identity.
func ConfigureCloudOrigins(localhost bool) error {
	if err := Provision(CloudRoot); err != nil {
		return err
	}
	origins := domainOrigins
	ports := []string{"127.0.0.1:443:8443"}
	if localhost {
		origins = localhostOrigins
		ports = []string{"127.0.0.1:9443:8443", "127.0.0.1:9444:8444"}
	}
	if err := writeJSON(filepath.Join(CloudRoot, "public-origins.json"), origins); err != nil {
		return err
	}
	return writeJSON(
		filepath.Join(CloudRoot, "gateway.json"),
		map[string]any{"services": map[string]any{"gateway": map[string]any{"ports": ports}}},
	)
}

func ensureCloudCertificates() error {
	for _, name := range []string{"cloud", "manufacturer"} {
		pair, err := tls.LoadX509KeyPair(
			filepath.Join(CloudRoot, "pki", name+".pem"),
			filepath.Join(CloudRoot, "pki", name+".key"),
		)
		if err != nil {
			return err
		}
		if pair.Leaf.VerifyHostname(name+".thermora.io") != nil || time.Until(pair.Leaf.NotAfter) < 7*24*time.Hour {
			return RenewCertificates(filepath.Join(CloudRoot, "pki"))
		}
	}
	return nil
}

// domainHosts reclaims canonical Thermora loopback entries when local state is recreated.
// Uninstall removes only this CA's entry; custom or non-loopback mappings remain conflicts.
func domainHosts(data []byte, fingerprint string, install bool) ([]byte, error) {
	prefix := "127.0.0.1 cloud.thermora.io manufacturer.thermora.io # thermora-local:"
	owned := prefix + fingerprint
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	result := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		if line == owned {
			continue
		}
		if install {
			if previous, ok := strings.CutPrefix(line, prefix); ok && len(previous) == sha256.Size*2 {
				if _, err := hex.DecodeString(previous); err == nil {
					continue
				}
			}
			for _, field := range strings.Fields(strings.SplitN(line, "#", 2)[0]) {
				if strings.EqualFold(field, "cloud.thermora.io") ||
					strings.EqualFold(field, "manufacturer.thermora.io") {
					return nil, errors.New(
						"a Thermora hostname already exists in hosts outside this installation; resolve the conflict before installing",
					)
				}
			}
		}
		result = append(result, line)
	}
	if install {
		result = append(result, owned)
	}
	return []byte(strings.Join(result, "\n") + "\n"), nil
}

// InstallCloudDomain is invoked explicitly under sudo by the local-install/uninstall tasks.
// It never accesses the CA private key or changes unrelated host entries/certificates.
func InstallCloudDomain(ctx context.Context, install bool) error {
	if os.Geteuid() != 0 {
		return errors.New("use task cloud:local-install or cloud:local-uninstall for the explicit administrator step")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return errors.New(
			"automatic trust setup supports macOS and Debian/Ubuntu; see README.md for the localhost option",
		)
	}
	certPEM, err := os.ReadFile(filepath.Join(CloudRoot, "pki/ca.pem"))
	if err != nil {
		return err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return errors.New("invalid local CA PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	if !cert.IsCA || (install && time.Now().After(cert.NotAfter)) {
		return errors.New("invalid or expired local CA")
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(cert.Raw))
	hosts, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return err
	}
	updated, err := domainHosts(hosts, fingerprint, install)
	if err != nil {
		return err
	}
	// Fixed executables and arguments; no shell or ambient executable lookup under sudo.
	run := func(command string, args ...string) error {
		// #nosec G204 -- executable and arguments are fixed OS trust operations; certificate paths/hashes are locally derived.
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	if runtime.GOOS == "darwin" {
		if install {
			path, pathErr := filepath.Abs(filepath.Join(CloudRoot, "pki/ca.pem"))
			if pathErr != nil {
				return pathErr
			}
			err = run(
				"/usr/bin/security",
				"add-trusted-cert",
				"-d",
				"-r",
				"trustRoot",
				"-k",
				"/Library/Keychains/System.keychain",
				path,
			)
		} else {
			// #nosec G401 -- SHA-1 is the Keychain certificate selector, not a security primitive here.
			selector := fmt.Sprintf("%X", sha1.Sum(cert.Raw))
			// Absence is already the desired state; check the exact fingerprint before deletion.
			output, findErr := exec.CommandContext(
				ctx,
				"/usr/bin/security",
				"find-certificate",
				"-a",
				"-Z",
				"/Library/Keychains/System.keychain",
			).Output()
			if findErr != nil {
				return findErr
			}
			if bytes.Contains(output, []byte(selector)) {
				err = run(
					"/usr/bin/security",
					"delete-certificate",
					"-Z",
					selector,
					"/Library/Keychains/System.keychain",
				)
			}
		}
	} else {
		target := filepath.Join("/usr/local/share/ca-certificates", "thermora-"+fingerprint+".crt")
		// #nosec G304 G703 -- target is a fixed trust directory plus a hex SHA-256 fingerprint, with no path separators from input.
		existing, readErr := os.ReadFile(target)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		if readErr == nil && !bytes.Equal(existing, certPEM) {
			return errors.New("existing trust file differs; refusing to replace it")
		}
		if install {
			// #nosec G306 G703 -- public certificate; target contains only a fixed directory and hex SHA-256 fingerprint.
			err = os.WriteFile(target, certPEM, 0o644)
		} else if readErr == nil {
			// #nosec G703 -- target is fixed trust directory plus hex SHA-256; matching contents were checked above.
			err = os.Remove(target)
		}
		if err == nil {
			err = run("/usr/sbin/update-ca-certificates")
		}
	}
	if err != nil {
		return fmt.Errorf("update local certificate trust: %w", err)
	}
	// Existing inode and permissions are retained (including on Docker Desktop hosts).
	// #nosec G306 -- /etc/hosts is public host resolution data, not a secret.
	if err = os.WriteFile("/etc/hosts", updated, 0o644); err != nil {
		return fmt.Errorf("trust updated but hosts update failed: %w", err)
	}
	_, err = fmt.Fprintln(
		os.Stdout,
		"Updated only this installation's Thermora host entries and CA trust. Restart your browser if it cached DNS or certificate errors.",
	)
	return err
}

// CloudDoctor uses system trust and normal resolver behavior, just as a browser does.
func CloudDoctor(ctx context.Context) error {
	origins, err := ReadCloudOrigins()
	if err != nil {
		return err
	}
	if err = CloudDNS(ctx); err != nil {
		return err
	}
	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origins.Cloud+"/health/ready", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("local HTTPS unavailable or not trusted (run cloud:up and cloud:local-install): %w", err)
	}
	closeErr := response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errors.Join(fmt.Errorf("cloud not ready: HTTP %d", response.StatusCode), closeErr)
	}
	return closeErr
}

// CloudDNS fails before startup or an HTTP request can target a public namesake.
func CloudDNS(ctx context.Context) error {
	origins, err := ReadCloudOrigins()
	if err != nil {
		return err
	}
	if origins == domainOrigins {
		for _, host := range []string{"cloud.thermora.io", "manufacturer.thermora.io"} {
			ips, lookupErr := net.DefaultResolver.LookupIPAddr(ctx, host)
			if lookupErr != nil || len(ips) == 0 {
				return fmt.Errorf("%s does not resolve locally; run task cloud:local-install", host)
			}
			for _, ip := range ips {
				if !ip.IP.IsLoopback() {
					return fmt.Errorf("%s resolves outside loopback; repair local hosts setup", host)
				}
			}
		}
	}
	return nil
}
