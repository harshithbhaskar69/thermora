package local

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"thermora.local/thermora/internal/messaging"
)

func Secret() string { return rand.Text() + rand.Text() }

// Provision creates local secrets once. It never silently rotates existing device trust.
func Provision(root string) error {
	// Also add this directory to existing installations without rotating their identity.
	if err := os.MkdirAll(filepath.Join(root, "devices"), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "cloud.json")); err == nil {
		return errors.New("incomplete local provisioning; inspect .local before retrying")
	}
	for _, dir := range []string{"", "pki", "mqtt", "edge-state", "sim-state"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			return err
		}
	}
	if err := certificates(filepath.Join(root, "pki")); err != nil {
		return err
	}
	adminPass, cloudPass, pgPass, appPass := Secret(), Secret(), Secret(), Secret()
	cloud := CloudConfig{
		DatabaseURL: "postgres://thermora_app:" + url.QueryEscape(appPass) + "@postgres:5432/thermora?sslmode=disable",
		MQTT: messaging.Settings{
			URL:      "tls://mqtt:8883",
			Username: "cloud",
			Password: cloudPass,
			ClientID: "cloud",
			CAFile:   "/run/pki/ca.pem",
		},
		Admin: messaging.Settings{
			URL:      "tls://mqtt:8883",
			Username: "admin",
			Password: adminPass,
			ClientID: "cloud-admin",
			CAFile:   "/run/pki/ca.pem",
		},
		Certificate: "/run/pki/cloud.pem",
		Key:         "/run/pki/cloud.key",
	}
	migration := MigrationConfig{
		DatabaseURL: "postgres://thermora:" + url.QueryEscape(
			pgPass,
		) + "@postgres:5432/thermora?sslmode=disable",
		ApplicationPassword: appPass,
	}
	seed := Seed{
		OrganizationID: uuid.NewString(),
		UserID:         uuid.NewString(),
		Login:          "owner@thermora.local",
		Password:       Secret(),
		DeviceID:       uuid.NewString(),
		RunID:          uuid.NewString(),
		Bootstrap:      Secret(),
	}
	edge := EdgeConfig{
		DeviceID:  seed.DeviceID,
		RunID:     seed.RunID,
		RunEpoch:  1,
		Bootstrap: seed.Bootstrap,
		CloudURL:  "https://cloud:8443",
		CAFile:    "/run/pki/ca.pem",
		MQTTURL:   "tls://mqtt:8883",
		ModbusURL: "tcp://sim:1502",
	}
	for name, value := range map[string]any{"cloud.json": cloud, "migrate.json": migration, "edge.json": edge, "seed.json": seed} {
		if err := writeJSON(filepath.Join(root, name), value); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(root, "postgres-password"), []byte(pgPass), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(
		filepath.Join(root, "compose.env"),
		fmt.Appendf(nil, "THERMORA_UID=%d\nTHERMORA_GID=%d\n", os.Getuid(), os.Getgid()),
		0o600,
	); err != nil {
		return err
	}
	if err := brokerConfig(filepath.Join(root, "mqtt", "dynamic-security.json"), adminPass, cloudPass); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "ready"), []byte("local resources provisioned\n"), 0o600)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func certificates(dir string) error {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial := func() *big.Int {
		n, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if e != nil {
			panic(e)
		}
		return n
	}
	ca := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Thermora local development CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	// #nosec G306 -- this file contains only the public CA certificate; the private key below is 0600.
	if err = os.WriteFile(
		filepath.Join(dir, "ca.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		0o644,
	); err != nil {
		return err
	}
	// The CA key is needed only for deliberate future renewal and never mounted in a container.
	keyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return err
	}
	if err = os.WriteFile(
		filepath.Join(dir, "ca.key"),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		0o600,
	); err != nil {
		return err
	}
	return RenewCertificates(dir)
}

// RenewCertificates renews only server certificates under the existing local CA.
// Callers must restart cloud and broker after replacing their certificates.
func RenewCertificates(dir string) error {
	return serverCertificates(dir, []string{"cloud", "mqtt", "manufacturer"})
}

func serverCertificates(dir string, names []string) error {
	// #nosec G304 -- dir is the operator's local PKI directory, never a remote request parameter.
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return err
	}
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil {
		return errors.New("invalid CA certificate")
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return err
	}
	// #nosec G304 -- the private key is read only from the same operator-selected PKI directory.
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return errors.New("invalid CA key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return err
	}
	caKey, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return errors.New("unsupported CA key")
	}
	now := time.Now()
	if !now.Before(ca.NotAfter) {
		return errors.New("local CA expired; provision a new local environment")
	}
	for _, name := range names {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return err
		}
		cert := &x509.Certificate{
			SerialNumber: serial,
			Subject:      pkix.Name{CommonName: name},
			DNSNames:     []string{name, name + ".localhost", "localhost", "floci-eks-thermora-cloud"},
			IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.AddDate(0, 3, 0),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		if name == "cloud" || name == "manufacturer" {
			cert.DNSNames = append(cert.DNSNames, name+".thermora.io")
		}
		if cert.NotAfter.After(ca.NotAfter) {
			cert.NotAfter = ca.NotAfter
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		if err != nil {
			return err
		}
		// #nosec G306 -- server certificates are public; their separate private key files are 0600.
		if err = os.WriteFile(
			filepath.Join(dir, name+".pem"),
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			0o644,
		); err != nil {
			return err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		if err = os.WriteFile(
			filepath.Join(dir, name+".key"),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
			0o600,
		); err != nil {
			return err
		}
	}
	return nil
}

func brokerConfig(path, adminPassword, cloudPassword string) error {
	client := func(name, id, password string) (map[string]any, error) {
		salt := make([]byte, 12)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		hash, err := pbkdf2.Key(sha512.New, password, salt, 10000, 64)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"username":   name,
			"clientid":   id,
			"salt":       base64.StdEncoding.EncodeToString(salt),
			"password":   base64.StdEncoding.EncodeToString(hash),
			"iterations": 10000,
			"roles":      []any{map[string]any{"rolename": name}},
		}, nil
	}
	admin, err := client("admin", "cloud-admin", adminPassword)
	if err != nil {
		return err
	}
	cloud, err := client("cloud", "cloud", cloudPassword)
	if err != nil {
		return err
	}
	acl := func(kind, topic string) map[string]any {
		return map[string]any{"acltype": kind, "topic": topic, "allow": true}
	}
	adminACL := []any{
		acl("publishClientSend", messaging.ControlTopic),
		acl("publishClientReceive", messaging.ControlTopic+"/response"),
		acl("subscribeLiteral", messaging.ControlTopic+"/response"),
	}
	cloudACL := messaging.CloudACL()
	return writeJSON(
		path,
		map[string]any{
			"clients": []any{admin, cloud},
			"roles": []any{
				map[string]any{"rolename": "admin", "acls": adminACL},
				map[string]any{"rolename": "cloud", "acls": cloudACL},
			},
			"defaultACLAccess": map[string]bool{
				"publishClientSend":    false,
				"publishClientReceive": false,
				"subscribe":            false,
				"unsubscribe":          false,
			},
		},
	)
}
