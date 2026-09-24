package manufacturer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"

	"thermora.local/thermora/internal/messaging"
	"thermora.local/thermora/internal/protocol"
)

// Message preserves equipment evidence IDs across the HTTPS/MQTT boundary.
type Message struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

type Point struct {
	Type       string     `json:"type"`
	Value      any        `json:"value,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	Name       string     `json:"name"`
	Unit       string     `json:"unit"`
	Readable   bool       `json:"readable"`
	Writable   bool       `json:"writable"`
}

type EquipmentState struct {
	DeviceID string           `json:"device_id"`
	Messages []Message        `json:"messages"`
	Points   []Point          `json:"points"`
	Latest   *protocol.Sample `json:"latest,omitempty"`
}

func envelopeID(kind string, payload []byte) string {
	hash := sha256.Sum256(append([]byte(kind), payload...))
	return hex.EncodeToString(hash[:])
}

func equipmentPoints(s *protocol.Sample) []Point {
	points := []Point{}
	add := func(name, unit, kind string, writable bool, value any) {
		p := Point{Name: name, Unit: unit, Type: kind, Readable: true, Writable: writable}
		if s != nil {
			p.Value = value
			p.ObservedAt = &s.ObservedAt
		}
		points = append(points, p)
	}
	var sample protocol.Sample
	if s != nil {
		sample = *s
	}
	add("supply_setpoint", "degC", "number", true, sample.SupplyTarget)
	add("room_target", "degC", "number", true, sample.RoomTarget)
	add("supply_temperature", "degC", "number", false, sample.Supply)
	add("indoor_temperature", "degC", "number", false, sample.Indoor)
	add("outdoor_temperature", "degC", "number", false, sample.Outdoor)
	add("electrical_power", "W", "integer", false, sample.ElectricalW)
	add("electrical_energy", "Wh", "integer", false, sample.EnergyWh)
	add("thermal_energy", "Wh", "integer", false, sample.ThermalEnergyWh)
	add("alarms", "code", "array", false, sample.Alarms)
	if sample.HotWater != nil && sample.Buffer != nil {
		add("buffer_temperature", "degC", "number", false, *sample.Buffer)
		add("hot_water_temperature", "degC", "number", false, sample.HotWater.Temperature)
		add("hot_water_start", "degC", "number", true, sample.HotWater.Start)
		add("hot_water_stop", "degC", "number", true, sample.HotWater.Stop)
		add("hot_water_electrical_energy", "Wh", "integer", false, sample.HotWater.EnergyWh)
	}
	return points
}

// StartEquipment connects only the fixture's tenant-scoped broker principal.
func (m *Mock) StartEquipment(ctx context.Context) error {
	if m.config.MQTT.URL == "" {
		return nil
	}
	topics := []string{}
	for _, kind := range []string{"telemetry", "command/event", "config/reported", "demo/event", "hello"} {
		topics = append(topics, protocol.Topic(m.config.OrganizationID, "+", kind))
	}
	var err error
	m.mqtt, err = messaging.Connect(ctx, m.config.MQTT, topics, nil)
	return err
}

func (m *Mock) RunEquipment(ctx context.Context) {
	if m.mqtt == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-m.mqtt.Incoming:
			if err := m.record(msg); err != nil {
				slog.Warn("manufacturer evidence deferred", "error", err)
			}
		}
	}
}

func (m *Mock) owns(device string) bool {
	for _, d := range m.config.Devices {
		if d.ID == device {
			return true
		}
	}
	return false
}

func (m *Mock) record(msg messaging.Message) error {
	parts := strings.Split(msg.Topic, "/")
	if len(parts) < 7 || len(msg.Payload) > protocol.MaxMessageBytes || parts[3] != m.config.OrganizationID ||
		!m.owns(parts[5]) {
		return errors.New("manufacturer equipment scope mismatch")
	}
	kind := strings.Join(parts[6:], "/")
	var sample *protocol.Sample
	switch kind {
	case "telemetry":
		var b protocol.Batch
		if err := protocol.Decode(msg.Payload, &b); err != nil {
			return err
		}
		if err := b.Validate(); err != nil {
			return err
		}
		if b.DeviceID != parts[5] {
			return errors.New("manufacturer batch identity mismatch")
		}
		sample = &b.Samples[len(b.Samples)-1]
	case "command/event", "config/reported", "demo/event", "hello":
		if !json.Valid(msg.Payload) {
			return errors.New("invalid equipment JSON")
		}
	default:
		return errors.New("unsupported equipment evidence")
	}
	entry := Message{ID: envelopeID(kind, msg.Payload), Kind: kind, Payload: msg.Payload}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return m.db.Update(func(tx *bolt.Tx) error {
		inbox, err := tx.CreateBucketIfNotExists([]byte("equipment:" + parts[5]))
		if err != nil {
			return err
		}
		if inbox.Get([]byte(entry.ID)) != nil {
			return nil
		}
		// ponytail: bounded per-device scan at 1,024 envelopes; use indexed cursors for a larger OEM fleet.
		if inbox.Stats().KeyN >= 1024 || inbox.Stats().LeafInuse+len(data) > 4*1024*1024 {
			return errors.New("manufacturer inbox full; edge retains unacknowledged evidence")
		}
		if err = inbox.Put([]byte(entry.ID), data); err != nil {
			return err
		}
		if sample != nil {
			latest, err := tx.CreateBucketIfNotExists([]byte("latest"))
			if err != nil {
				return err
			}
			var old protocol.Sample
			if previous := latest.Get([]byte(parts[5])); previous != nil {
				if err = json.Unmarshal(previous, &old); err != nil {
					return err
				}
			}
			if sample.RecordedAt.After(old.RecordedAt) || old.Sequence == 0 {
				encoded, err := json.Marshal(sample)
				if err != nil {
					return err
				}
				if err = latest.Put([]byte(parts[5]), encoded); err != nil {
					return err
				}
				return latest.Put([]byte(parts[5]+":message"), data)
			}
		}
		return nil
	})
}

func (m *Mock) authorizedDevice(w http.ResponseWriter, r *http.Request, control bool) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		http.Error(w, "Authorization required.", http.StatusUnauthorized)
		return false
	}
	err := m.db.View(func(tx *bolt.Tx) error {
		g, e := readGrant(tx, token, "access")
		if e != nil {
			return e
		}
		if control && g.Scope != "devices:read devices:control" {
			return ErrAuthorization
		}
		return nil
	})
	if err != nil {
		http.Error(w, "Reconnect the manufacturer account to authorize equipment control.", http.StatusUnauthorized)
		return false
	}
	if !m.owns(r.PathValue("device")) {
		http.NotFound(w, r)
		return false
	}
	return true
}

func (m *Mock) equipmentState(w http.ResponseWriter, r *http.Request) {
	if !m.authorizedDevice(w, r, false) {
		return
	}
	device := r.PathValue("device")
	result := EquipmentState{DeviceID: device, Messages: []Message{}, Points: []Point{}}
	err := m.db.View(func(tx *bolt.Tx) error {
		var newest Message
		if latest := tx.Bucket([]byte("latest")); latest != nil {
			if data := latest.Get([]byte(device)); data != nil {
				if err := json.Unmarshal(data, &result.Latest); err != nil {
					return err
				}
			}
			if data := latest.Get([]byte(device + ":message")); data != nil {
				if err := json.Unmarshal(data, &newest); err != nil {
					return err
				}
				result.Messages = append(result.Messages, newest)
			}
		}
		b := tx.Bucket([]byte("equipment:" + device))
		if b == nil {
			return nil
		}
		cursor := b.Cursor()
		size := 0
		for key, data := cursor.First(); key != nil; key, data = cursor.Next() {
			if len(result.Messages) >= 25 || size+len(data) >= 512*1024 {
				break
			}
			if string(key) == newest.ID {
				continue
			}
			var entry Message
			if err := json.Unmarshal(data, &entry); err != nil {
				return err
			}
			result.Messages = append(result.Messages, entry)
			size += len(data)
		}
		return nil
	})
	if err != nil {
		http.Error(w, "Equipment state unavailable.", http.StatusServiceUnavailable)
		return
	}
	result.Points = equipmentPoints(result.Latest)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (m *Mock) equipmentReceipt(w http.ResponseWriter, r *http.Request) {
	if !m.authorizedDevice(w, r, false) {
		return
	}
	var receipt struct {
		ID string `json:"id"`
	}
	if err := decodeEnvelope(r.Body, &receipt); err != nil || len(receipt.ID) != 64 {
		http.Error(w, "Invalid receipt.", http.StatusUnprocessableEntity)
		return
	}
	err := m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("equipment:" + r.PathValue("device")))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(receipt.ID))
	})
	if err != nil {
		http.Error(w, "Receipt unavailable.", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateControl(device string, message Message) error {
	switch message.Kind {
	case "demo/action":
		var op protocol.RunOperation
		if err := protocol.Decode(message.Payload, &op); err != nil {
			return err
		}
		return op.Validate(device, time.Now())

	case "command":
		var c protocol.Command
		if err := protocol.Decode(message.Payload, &c); err != nil {
			return err
		}
		cfg := protocol.NewConfiguration(device, c.RunID, protocol.Comfort{Min: 16, Target: 21, Max: 26})
		cfg.Version, cfg.Generation = c.ConfigVersion, c.Generation
		if _, err := uuid.Parse(c.RunID); err != nil || c.RunEpoch == 0 || c.ConfigVersion == 0 || c.Generation == 0 {
			return errors.New("invalid command identity")
		}
		return c.Validate(cfg, c.RunEpoch, c.ConfigVersion, 0, time.Now())
	case "config/desired":
		var c protocol.Configuration
		if err := protocol.Decode(message.Payload, &c); err != nil {
			return err
		}
		return c.Validate(device, c.RunID)
	case "control/lease":
		var l protocol.Lease
		if err := protocol.Decode(message.Payload, &l); err != nil {
			return err
		}
		_, identityErr := uuid.Parse(l.RunID)
		if identityErr != nil || l.Sequence == 0 || l.Generation == 0 || l.DeviceID != device || l.SchemaVersion != 1 ||
			!time.Now().Before(l.ValidUntil) ||
			!l.ValidUntil.After(l.IssuedAt) ||
			l.IssuedAt.After(time.Now().Add(5*time.Second)) ||
			l.ValidUntil.Sub(l.IssuedAt) > 30*time.Second {
			return errors.New("invalid or expired lease")
		}
	case "telemetry/receipt", "command/event/receipt":
		var receipt protocol.Receipt
		if err := protocol.Decode(message.Payload, &receipt); err != nil {
			return err
		}
		if receipt.SchemaVersion != 1 || (receipt.Status != "committed" && receipt.Status != "rejected") {
			return errors.New("invalid receipt")
		}
	default:
		return errors.New("unsupported control kind")
	}
	return nil
}

func (m *Mock) equipmentControl(w http.ResponseWriter, r *http.Request) {
	m.grantMu.RLock()
	defer m.grantMu.RUnlock()
	if !m.authorizedDevice(w, r, true) {
		return
	}
	var message Message
	if err := decodeEnvelope(
		r.Body,
		&message,
	); err != nil || len(message.ID) != 64 ||
		validateControl(r.PathValue("device"), message) != nil {
		http.Error(w, "Invalid or expired equipment instruction.", http.StatusUnprocessableEntity)
		return
	}
	if m.mqtt == nil || !m.mqtt.Ready.Load() {
		http.Error(w, "Equipment transport unavailable.", http.StatusServiceUnavailable)
		return
	}
	err := m.mqtt.Publish(
		r.Context(),
		protocol.Topic(m.config.OrganizationID, r.PathValue("device"), message.Kind),
		message.Payload,
		message.Kind == "config/desired",
	)
	if err != nil {
		http.Error(w, "Equipment transport unavailable.", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func decodeEnvelope(body io.Reader, out any) error {
	d := json.NewDecoder(body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing envelope data")
	}
	return nil
}

func (c *Client) State(ctx context.Context, token, device string) (EquipmentState, error) {
	var result EquipmentState
	err := c.equipmentRequest(ctx, token, http.MethodGet, device, "state", nil, &result)
	if err == nil && (result.DeviceID != device || len(result.Messages) > 25) {
		err = errors.New("invalid manufacturer state")
	}
	return result, err
}

func (c *Client) Send(ctx context.Context, token, device, kind string, payload []byte) error {
	return c.equipmentRequest(
		ctx,
		token,
		http.MethodPost,
		device,
		"control",
		Message{ID: envelopeID(kind, payload), Kind: kind, Payload: payload},
		nil,
	)
}

func (c *Client) Receipt(ctx context.Context, token, device, id string) error {
	return c.equipmentRequest(ctx, token, http.MethodPost, device, "receipts", struct {
		ID string `json:"id"`
	}{id}, nil)
}

func (c *Client) equipmentRequest(ctx context.Context, token, method, device, operation string, body, out any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	r, err := http.NewRequestWithContext(
		ctx,
		method,
		c.Config.APIURL+"/devices/"+url.PathEscape(device)+"/"+operation,
		bytes.NewReader(data),
	)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	return c.read(r, out)
}
