package local

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const CloudRoot = ".local/cloud"

// ProvisionCloud keeps Kubernetes identities and volumes separate from the Compose demo.
func ProvisionCloud() error {
	if err := Provision(CloudRoot); err != nil {
		return err
	}
	if err := ProvisionOperator(CloudRoot); err != nil {
		return err
	}
	origins, err := ReadCloudOrigins()
	if err != nil {
		return err
	}
	if err := ensureCloudCertificates(); err != nil {
		return err
	}
	if err := ProvisionOnboarding(CloudRoot, "", origins.Cloud, origins.Manufacturer); err != nil {
		return err
	}
	var cloud CloudConfig
	if err := Read(filepath.Join(CloudRoot, "cloud.json"), &cloud); err != nil {
		return err
	}
	cloud.ArtifactsEndpoint = "http://floci:4566"
	if err := writeJSON(filepath.Join(CloudRoot, "cloud.json"), cloud); err != nil {
		return err
	}
	var edge EdgeConfig
	if err := Read(filepath.Join(CloudRoot, "edge.json"), &edge); err != nil {
		return err
	}
	edge.CloudURL, edge.MQTTURL = "https://floci-eks-thermora-cloud:30443", "tls://floci-eks-thermora-cloud:31883"
	if err := writeJSON(filepath.Join(CloudRoot, "edge.json"), edge); err != nil {
		return err
	}
	data := make(map[string]string)
	brokerConfig, err := os.ReadFile("deploy/compose/mosquitto.conf")
	if err != nil {
		return err
	}
	data["mosquitto.conf"] = base64.StdEncoding.EncodeToString(brokerConfig)
	for name, path := range map[string]string{
		"operator-access.json":  "operator-access.json",
		"cloud.json":            "cloud.json",
		"migrate.json":          "migrate.json",
		"onboarding.json":       "onboarding.json",
		"manufacturer.json":     "manufacturer/config.json",
		"postgres-password":     "postgres-password",
		"dynamic-security.json": "mqtt/dynamic-security.json",
		"ca.pem":                "pki/ca.pem",
		"cloud.pem":             "pki/cloud.pem",
		"cloud.key":             "pki/cloud.key",
		"mqtt.pem":              "pki/mqtt.pem",
		"mqtt.key":              "pki/mqtt.key",
		"manufacturer.pem":      "pki/manufacturer.pem",
		"manufacturer.key":      "pki/manufacturer.key",
	} {
		// #nosec G304 -- both paths come from this fixed local provisioning manifest, never external input.
		content, err := os.ReadFile(filepath.Join(CloudRoot, path))
		if err != nil {
			return err
		}
		data[name] = base64.StdEncoding.EncodeToString(content)
	}
	return writeJSON(
		filepath.Join(CloudRoot, "secret.json"),
		map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]string{"name": "thermora-local", "namespace": "thermora"},
			"type":       "Opaque",
			"data":       data,
		},
	)
}

type clusterResources struct {
	VPC     string
	Subnets []string
}

// ProvisionCluster exercises only Floci's loopback EKS/EC2/IAM APIs. It cannot target AWS.
// Network resources are emulator metadata; Docker supplies the actual local network.
func ProvisionCluster(ctx context.Context) error {
	if _, err := flociRequest(ctx, "GET", "/clusters/thermora-cloud", nil, ""); err == nil {
		return waitCluster(ctx)
	} else if !errors.Is(err, errFlociNotFound) {
		return err
	}
	var resources clusterResources
	path := filepath.Join(CloudRoot, "resources.json")
	if err := Read(path, &resources); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if resources.VPC == "" {
		response, err := flociQuery(ctx, "CreateVpc", "2016-11-15", url.Values{"CidrBlock": {"10.60.0.0/16"}})
		if err != nil {
			return err
		}
		var result struct {
			VPC struct {
				ID string `xml:"vpcId"`
			} `xml:"vpc"`
		}
		if err = xml.Unmarshal(response, &result); err != nil {
			return err
		}
		if result.VPC.ID == "" {
			return errors.New("floci did not return a VPC identifier")
		}
		resources.VPC = result.VPC.ID
		if err = writeJSON(path, resources); err != nil {
			return err
		}
	}
	for len(resources.Subnets) < 2 {
		i := len(resources.Subnets)
		response, err := flociQuery(
			ctx,
			"CreateSubnet",
			"2016-11-15",
			url.Values{
				"VpcId":            {resources.VPC},
				"CidrBlock":        {fmt.Sprintf("10.60.%d.0/24", i+1)},
				"AvailabilityZone": {[]string{"us-east-1a", "us-east-1b"}[i]},
			},
		)
		if err != nil {
			return err
		}
		var result struct {
			Subnet struct {
				ID string `xml:"subnetId"`
			} `xml:"subnet"`
		}
		if err = xml.Unmarshal(response, &result); err != nil {
			return err
		}
		if result.Subnet.ID == "" {
			return errors.New("floci did not return a subnet identifier")
		}
		resources.Subnets = append(resources.Subnets, result.Subnet.ID)
		if err = writeJSON(path, resources); err != nil {
			return err
		}
	}
	if _, err := flociQuery(
		ctx,
		"GetRole",
		"2010-05-08",
		url.Values{"RoleName": {"thermora-cloud-local"}},
	); errors.Is(err, errFlociNotFound) {
		if _, err = flociQuery(
			ctx,
			"CreateRole",
			"2010-05-08",
			url.Values{
				"RoleName": {"thermora-cloud-local"},
				"AssumeRolePolicyDocument": {
					`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"eks.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
				},
			},
		); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	body, err := json.Marshal(
		map[string]any{
			"name":               "thermora-cloud",
			"version":            "1.36",
			"roleArn":            "arn:aws:iam::000000000000:role/thermora-cloud-local",
			"resourcesVpcConfig": map[string]any{"subnetIds": resources.Subnets},
			"tags":               map[string]string{"project": "thermora", "environment": "local"},
		},
	)
	if err != nil {
		return err
	}
	if _, err = flociRequest(ctx, "POST", "/clusters", body, "application/json"); err != nil {
		return err
	}
	return waitCluster(ctx)
}

func waitCluster(ctx context.Context) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		data, err := flociRequest(ctx, "GET", "/clusters/thermora-cloud", nil, "")
		if err != nil {
			return err
		}
		var result struct {
			Cluster struct {
				Status string `json:"status"`
			} `json:"cluster"`
		}
		if err = json.Unmarshal(data, &result); err != nil {
			return err
		}
		if result.Cluster.Status == "ACTIVE" {
			return nil
		}
		if result.Cluster.Status != "CREATING" {
			return fmt.Errorf("local EKS cluster status is %s", result.Cluster.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

var errFlociNotFound = errors.New("floci resource not found")

func flociQuery(ctx context.Context, action, version string, values url.Values) ([]byte, error) {
	values.Set("Action", action)
	values.Set("Version", version)
	return flociRequest(ctx, "POST", "/", []byte(values.Encode()), "application/x-www-form-urlencoded")
}

func flociRequest(
	ctx context.Context,
	method, path string,
	data []byte,
	contentType string,
) (result []byte, err error) {
	// Deliberately no ambient AWS configuration, endpoint override, proxy or redirects.
	client := &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	r, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:4566"+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(r)
	if err != nil {
		return nil, fmt.Errorf("local Floci request: %w", err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode == 404 {
		return nil, errFlociNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("local Floci returned HTTP %d", resp.StatusCode)
	}
	result, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return result, err
}
