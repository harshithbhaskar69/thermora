package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"thermora.local/thermora/internal/protocol"
)

const ControlTopic = "$CONTROL/dynamic-security/v1"

// Admin has a separate principal and a single in-flight broker management request.
type Admin struct {
	Client *Client
	mu     sync.Mutex
}

func (a *Admin) request(ctx context.Context, command map[string]any, allowExists bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := uuid.NewString()
	command["correlationData"] = id
	if err := a.Client.JSON(ctx, ControlTopic, map[string]any{"commands": []any{command}}, false); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-a.Client.Incoming:
			var reply struct {
				Responses []struct {
					Correlation string `json:"correlationData"`
					Error       string `json:"error"`
				} `json:"responses"`
			}
			if err := json.Unmarshal(msg.Payload, &reply); err != nil {
				return err
			}
			for _, r := range reply.Responses {
				if r.Correlation != id {
					continue
				}
				if r.Error != "" && (!allowExists || !strings.HasSuffix(strings.ToLower(r.Error), "already exists")) {
					return fmt.Errorf("broker administration: %s", r.Error)
				}
				return nil
			}
		}
	}
}

func (a *Admin) ProvisionDevice(ctx context.Context, org, device, password string) error {
	if err := a.ProvisionDeviceRole(ctx, org, device); err != nil {
		return err
	}
	role := "device-" + device
	if err := a.request(
		ctx,
		map[string]any{
			"command":  "createClient",
			"username": device,
			"clientid": device,
			"password": password,
			"roles":    []any{map[string]any{"rolename": role}},
		},
		true,
	); err != nil {
		return err
	}
	// Reconciliation uses the same attempt's password; an existing client is never rebound to another owner.
	return a.request(
		ctx,
		map[string]any{"command": "setClientPassword", "username": device, "password": password},
		false,
	)
}

// ProvisionManufacturer grants the fixture one tenant's equipment topics, never broker administration.
func (a *Admin) ProvisionManufacturer(ctx context.Context, org string, cfg Settings) error {
	if _, err := uuid.Parse(org); err != nil {
		return err
	}
	acls := []map[string]any{}
	for _, suffix := range []string{"telemetry", "command/event", "config/reported", "demo/event", "hello"} {
		for _, kind := range []string{"publishClientReceive", "subscribeLiteral", "unsubscribeLiteral"} {
			acls = append(
				acls,
				map[string]any{"acltype": kind, "topic": protocol.Topic(org, "+", suffix), "allow": true},
			)
		}
	}
	for _, suffix := range []string{"command", "config/desired", "demo/action", "control/lease", "telemetry/receipt", "command/event/receipt"} {
		acls = append(
			acls,
			map[string]any{"acltype": "publishClientSend", "topic": protocol.Topic(org, "+", suffix), "allow": true},
		)
	}
	role := "manufacturer-" + org
	if err := a.role(ctx, role, acls); err != nil {
		return err
	}
	if err := a.request(
		ctx,
		map[string]any{
			"command":  "createClient",
			"username": cfg.Username,
			"clientid": cfg.ClientID,
			"password": cfg.Password,
			"roles":    []any{map[string]any{"rolename": role}},
		},
		true,
	); err != nil {
		return err
	}
	return a.request(
		ctx,
		map[string]any{"command": "setClientPassword", "username": cfg.Username, "password": cfg.Password},
		false,
	)
}

func (a *Admin) ProvisionDeviceRole(ctx context.Context, org, device string) error {
	role := "device-" + device
	acls := []map[string]any{}
	for _, suffix := range []string{"telemetry", "command/event", "config/reported", "demo/event", "presence", "hello", "heartbeat"} {
		acls = append(
			acls,
			map[string]any{"acltype": "publishClientSend", "topic": protocol.Topic(org, device, suffix), "allow": true},
		)
	}
	for _, suffix := range []string{"telemetry/receipt", "command", "command/event/receipt", "config/desired", "demo/action", "control/lease"} {
		for _, kind := range []string{"publishClientReceive", "subscribeLiteral", "unsubscribeLiteral"} {
			acls = append(
				acls,
				map[string]any{"acltype": kind, "topic": protocol.Topic(org, device, suffix), "allow": true},
			)
		}
	}
	return a.role(ctx, role, acls)
}

func (a *Admin) role(ctx context.Context, name string, acls []map[string]any) error {
	if err := a.request(
		ctx,
		map[string]any{"command": "createRole", "rolename": name, "acls": acls},
		true,
	); err != nil {
		return err
	}
	return a.request(ctx, map[string]any{"command": "modifyRole", "rolename": name, "acls": acls}, false)
}

func CloudACL() []map[string]any {
	acls := []map[string]any{}
	for _, suffix := range []string{"telemetry", "command/event", "config/reported", "demo/event", "presence", "hello", "heartbeat"} {
		for _, kind := range []string{"publishClientReceive", "subscribeLiteral", "unsubscribeLiteral"} {
			acls = append(
				acls,
				map[string]any{"acltype": kind, "topic": "thermora/v1/org/+/device/+/" + suffix, "allow": true},
			)
		}
	}
	for _, suffix := range []string{"telemetry/receipt", "command", "command/event/receipt", "config/desired", "demo/action", "control/lease"} {
		acls = append(
			acls,
			map[string]any{
				"acltype": "publishClientSend",
				"topic":   "thermora/v1/org/+/device/+/" + suffix,
				"allow":   true,
			},
		)
	}
	return acls
}

func (a *Admin) ProvisionCloudRole(ctx context.Context) error {
	return a.role(ctx, "cloud", CloudACL())
}

// DisableDevice kicks active sessions as well as rejecting future authentication.
func (a *Admin) DisableDevice(ctx context.Context, device string) error {
	return a.request(ctx, map[string]any{"command": "disableClient", "username": device}, false)
}
