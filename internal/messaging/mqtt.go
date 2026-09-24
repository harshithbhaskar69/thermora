// Package messaging owns MQTT transport, not application delivery guarantees.
package messaging

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"

	"thermora.local/thermora/internal/protocol"
)

type Settings struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	ClientID string `json:"client_id"`
	CAFile   string `json:"ca_file"`
}

type Message struct {
	Topic   string
	Payload []byte
}
type Client struct {
	manager  *autopaho.ConnectionManager
	Incoming chan Message
	Ready    atomic.Bool
}

func TLSConfig(caFile string) (*tls.Config, error) {
	// #nosec G304 -- the CA path comes from operator-owned local configuration, not MQTT or HTTP input.
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read local CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid local CA")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, nil
}

func Connect(ctx context.Context, cfg Settings, topics []string, will *Message) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "tls" || u.Host == "" {
		return nil, errors.New("MQTT requires a tls:// endpoint")
	}
	tlsConfig, err := TLSConfig(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	c := &Client{Incoming: make(chan Message, 128)}
	config := autopaho.ClientConfig{
		ServerUrls: []*url.URL{
			u,
		},
		TlsCfg:                        tlsConfig,
		KeepAlive:                     10,
		CleanStartOnInitialConnection: true,
		SessionExpiryInterval:         0,
		ConnectTimeout:                5 * time.Second,
		ConnectUsername:               cfg.Username,
		ConnectPassword:               []byte(cfg.Password),
		ReconnectBackoff: func(attempt int) time.Duration {
			// #nosec G404 -- reconnect jitter needs distribution, not cryptographic unpredictability.
			return time.Duration(1<<min(attempt, 4))*time.Second + time.Duration(rand.IntN(1000))*time.Millisecond
		},
		OnConnectionDown: func() bool { c.Ready.Store(false); return true },
		OnConnectError:   func(err error) { slog.Warn("MQTT connection unavailable", "client_id", cfg.ClientID, "error", err) },
		ClientConfig: paho.ClientConfig{
			ClientID: cfg.ClientID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){func(p paho.PublishReceived) (bool, error) {
				if len(p.Packet.Payload) > protocol.MaxMessageBytes {
					return true, nil
				}
				select {
				case c.Incoming <- Message{p.Packet.Topic, append([]byte(nil), p.Packet.Payload...)}:
				default:
					slog.Warn("MQTT receive queue full; application retry required", "client_id", cfg.ClientID)
				}
				return true, nil
			}},
		},
	}
	if will != nil {
		config.WillMessage = &paho.WillMessage{Topic: will.Topic, Payload: will.Payload, QoS: 1, Retain: true}
	}
	config.OnConnectionUp = func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
		go func() {
			if len(topics) > 0 {
				subs := make([]paho.SubscribeOptions, 0, len(topics))
				for _, topic := range topics {
					subs = append(subs, paho.SubscribeOptions{Topic: topic, QoS: 1})
				}
				subCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				ack, err := cm.Subscribe(subCtx, &paho.Subscribe{Subscriptions: subs})
				if err != nil {
					slog.Error("MQTT subscribe failed", "error", err)
					return
				}
				for _, code := range ack.Reasons {
					if code >= 0x80 {
						slog.Error("MQTT subscription denied", "client_id", cfg.ClientID, "code", code)
						return
					}
				}
			}
			c.Ready.Store(true)
		}()
	}
	c.manager, err = autopaho.NewConnection(ctx, config)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) Wait(ctx context.Context) error {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		if c.Ready.Load() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (c *Client) Publish(ctx context.Context, topic string, payload []byte, retain bool) error {
	if len(payload) > protocol.MaxMessageBytes {
		return errors.New("MQTT payload too large")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	expiry := uint32(30)
	packet := &paho.Publish{
		Topic:      topic,
		Payload:    payload,
		QoS:        1,
		Retain:     retain,
		Properties: &paho.PublishProperties{ContentType: "application/json"},
	}
	if !retain {
		packet.Properties.MessageExpiry = &expiry
	}
	ack, err := c.manager.Publish(ctx, packet)
	if err != nil {
		return err
	}
	if ack.ReasonCode >= 0x80 {
		return fmt.Errorf("MQTT publish rejected: %d", ack.ReasonCode)
	}
	return nil
}

func (c *Client) JSON(ctx context.Context, topic string, value any, retain bool) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.Publish(ctx, topic, data, retain)
}
