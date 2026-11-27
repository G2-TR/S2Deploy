package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

type target struct{ File, Name, Group string }

const g2un = "g2un"
const masterRef = "refs/heads/master"
const g2Ref = "refs/heads/prod-merge"

var targets = map[string]target{
	g2un:      {"build-g2un.yml", "G2un.work 部署", "work"},
	"main":    {"build.yml", "81环境", "work"},
	"demo":    {"build-demo.yml", "演示环境", "work"},
	"work":    {"build-work.yml", "90环境", "work"},
	"g2-chis": {"build-chis.yml", "CHIS部署", "g2"},
	"g2-work": {"build-g2work.yml", "WORK部署", "g2"},
	"g2-trng": {"build-g2trng.yml", "TRNG部署", "g2"},
	"g2-plat": {"build-g2plat.yml", "PLAT部署", "g2"},
	"g2-call": {"build-g2call.yml", "CALL部署", "g2"},
	"g2-jzxt": {"build-g2jzxt.yml", "JZXT部署", "g2"},
}

type deployment struct {
	ID             string
	Target         string
	Phase          string
	Created        time.Time
	RunID          int64
	Started, Ended time.Time
	Progress       string
	Result         string

	CardID    string
	Sent      bool
	Sequence  int
	Delivered string

	ResultCardID string
	ResultSent   bool
}

type store struct{ db *bolt.DB }

var records = []byte("deployments")
var pending = []byte("pending")
var errUnknown = errors.New("unknown deployment")
var busy = errors.New("该目标已有部署正在执行")

func openStore(file string) (*store, error) {
	db, e := bolt.Open(file, 0600, &bolt.Options{Timeout: time.Second})
	if e != nil {
		return nil, e
	}
	e = db.Update(func(tx *bolt.Tx) error {
		for _, k := range [][]byte{records, pending, []byte("events"), []byte("targets")} {
			if _, err := tx.CreateBucketIfNotExists(k); err != nil {
				return err
			}
		}
		return nil
	})
	if e != nil {
		db.Close()
		return nil, e
	}
	return &store{db}, nil
}

func save(tx *bolt.Tx, d deployment) error {
	data, e := json.Marshal(d)
	if e != nil {
		return e
	}
	if e = tx.Bucket(records).Put([]byte(d.ID), data); e != nil {
		return e
	}
	if d.Phase == "done" {
		return tx.Bucket(pending).Delete([]byte(d.ID))
	}
	return tx.Bucket(pending).Put([]byte(d.ID), []byte{1})
}

func (s *store) request(event, id, key string) error {
	var refused bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		events, locks := tx.Bucket([]byte("events")), tx.Bucket([]byte("targets"))
		if receipt := events.Get([]byte(event)); receipt != nil {
			refused = string(receipt) == "busy"
			return nil
		}
		if locks.Get([]byte(key)) != nil {
			refused = true
			return events.Put([]byte(event), []byte("busy"))
		}
		if e := locks.Put([]byte(key), []byte(id)); e != nil {
			return e
		}
		if e := events.Put([]byte(event), []byte(id)); e != nil {
			return e
		}
		return save(tx, deployment{ID: id, Target: key, Phase: "queued", Created: time.Now().UTC()})
	})
	if err == nil && refused {
		return busy
	}
	return err
}

func (s *store) change(id string, fn func(*deployment) error) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var d deployment
		raw := tx.Bucket(records).Get([]byte(id))
		if raw == nil {
			return errUnknown
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return fmt.Errorf("unknown deployment %s: %w", id, err)
		}
		if err := fn(&d); err != nil {
			return err
		}
		locks := tx.Bucket([]byte("targets"))
		if d.Result != "" && string(locks.Get([]byte(d.Target))) == d.ID {
			if err := locks.Delete([]byte(d.Target)); err != nil {
				return err
			}
		}
		return save(tx, d)
	})
}

func (s *store) list() (items []deployment, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(pending).ForEach(func(k, v []byte) error {
			var d deployment
			if e := json.Unmarshal(tx.Bucket(records).Get(k), &d); e != nil {
				return e
			}
			items = append(items, d)
			return nil
		})
	})
	return
}

func (s *store) get(id string) (d deployment, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(records).Get([]byte(id))
		if raw == nil {
			return errUnknown
		}
		return json.Unmarshal(raw, &d)
	})
	return
}
