// Package edge translates authenticated cloud intent into validated local equipment operations.
package edge

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"

	"thermora.local/thermora/internal/local"
	"thermora.local/thermora/internal/protocol"
)

type (
	identityState struct{ Attempt, Password, Organization string }
	journalEntry  struct {
		Command       protocol.Command
		Stage, Reason string
		Frame         protocol.Frame
	}
)
type state struct{ db *bolt.DB }

func openState(path string) (*state, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"meta", "commands", "events", "batches"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &state{db}, nil
}

func (s *state) read(bucket, key string, value any) (bool, error) {
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte(bucket)).Get([]byte(key))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, value)
	})
	return found, err
}

func (s *state) put(bucket, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(bucket)).Put([]byte(key), data) })
}

func (s *state) remove(bucket, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(bucket)).Delete([]byte(key)) })
}

func (s *state) identity() (identityState, error) {
	var id identityState
	found, err := s.read("meta", "identity", &id)
	if err != nil {
		return id, err
	}
	if !found {
		id = identityState{Attempt: uuid.NewString(), Password: local.Secret()}
		err = s.put("meta", "identity", id)
	}
	return id, err
}

func (s *state) event(c protocol.Command, stage, reason string, elapsed uint32, frame *protocol.Frame) error {
	e := protocol.Event{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		CommandID:     c.ID,
		DeviceID:      c.DeviceID,
		RunID:         c.RunID,
		Stage:         stage,
		Reason:        reason,
		OccurredAt:    time.Now().UTC(),
		ObservedAt:    protocol.StartTime.Add(time.Duration(elapsed) * time.Second),
	}
	eventData, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		events, commands := tx.Bucket([]byte("events")), tx.Bucket([]byte("commands"))
		stats := events.Stats()
		if stats.LeafInuse+stats.BranchInuse+len(eventData) > 4*1024*1024 || stats.KeyN >= 10000 {
			return errors.New("command event journal full; remote control stopped")
		}
		if commands.Get([]byte(c.ID)) == nil && commands.Stats().KeyN >= 10000 {
			return errors.New("command journal full; remote control stopped")
		}
		entry := journalEntry{Command: c, Stage: stage, Reason: reason}
		if previous := commands.Get([]byte(c.ID)); previous != nil {
			var old journalEntry
			if err := json.Unmarshal(previous, &old); err != nil {
				return err
			}
			entry.Frame = old.Frame
		}
		if frame != nil {
			entry.Frame = *frame
		}
		entryData, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if err := commands.Put([]byte(c.ID), entryData); err != nil {
			return err
		}
		return events.Put([]byte(e.ID), eventData)
	})
}

type pendingMessage struct {
	key  string
	data []byte
}

func (s *state) pending(bucket string, limit int) ([]pendingMessage, error) {
	result := []pendingMessage{}
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucket)).Cursor()
		for k, v := c.First(); k != nil && len(result) < limit; k, v = c.Next() {
			result = append(result, pendingMessage{string(k), append([]byte(nil), v...)})
		}
		return nil
	})
	return result, err
}

func (s *state) batch(b protocol.Batch) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("batches"))
		stats := bucket.Stats()
		if stats.LeafInuse+stats.BranchInuse+len(data) > 64*1024*1024 {
			return errors.New("telemetry spool full")
		}
		return bucket.Put([]byte(fmt.Sprintf("%010d:%s", b.Samples[0].Elapsed, b.ID)), data)
	})
}

func (s *state) ackBatch(id string) error {
	// ponytail: bounded linear scan for one device; index batch IDs when fleet/backlog measurements justify it.
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("batches"))
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var batch protocol.Batch
			if err := json.Unmarshal(v, &batch); err != nil {
				return err
			}
			if batch.ID == id {
				return c.Delete()
			}
		}
		return nil
	})
}

func (s *state) diagnostics(now time.Time) (protocol.Diagnostics, error) {
	d := protocol.Diagnostics{RecordedAt: now.UTC()}
	err := s.db.View(func(tx *bolt.Tx) error {
		stats := tx.Bucket([]byte("batches")).Stats()
		d.BufferedBatches = stats.KeyN
		d.BufferedBytes = stats.LeafInuse + stats.BranchInuse
		d.PendingEvents = tx.Bucket([]byte("events")).Stats().KeyN
		d.JournalEntries = tx.Bucket([]byte("commands")).Stats().KeyN
		return nil
	})
	return d, err
}
