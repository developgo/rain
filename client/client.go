// Package client provides a torrent client implementation that is capable of downlaoding multiple torrents in parallel.
package client

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/boltdb/bolt"
	"github.com/cenkalti/rain/internal/logger"
	"github.com/cenkalti/rain/torrent"
	"github.com/cenkalti/rain/torrent/resumer/boltdbresumer"
	"github.com/cenkalti/rain/torrent/storage"
	"github.com/cenkalti/rain/torrent/storage/filestorage"
	"github.com/mitchellh/go-homedir"
)

var mainBucket = []byte("torrents")

type Client struct {
	config   Config
	db       *bolt.DB
	log      logger.Logger
	torrents map[uint64]*Torrent
	m        sync.RWMutex
}

// New returns a pointer to new Rain BitTorrent client.
func New(cfg Config) (*Client, error) {
	var err error
	cfg.Database, err = homedir.Expand(cfg.Database)
	if err != nil {
		return nil, err
	}
	cfg.DataDir, err = homedir.Expand(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	err = os.MkdirAll(filepath.Dir(cfg.Database), 0750)
	if err != nil {
		return nil, err
	}
	db, err := bolt.Open(cfg.Database, 0640, nil)
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err2 := tx.CreateBucketIfNotExists(mainBucket)
		return err2
	})
	if err != nil {
		return nil, err
	}
	// TODO load existing torrents on startup
	return &Client{
		config:   cfg,
		db:       db,
		log:      logger.New("client"),
		torrents: make(map[uint64]*Torrent),
	}, nil
}

func (c *Client) Close() error {
	c.m.Lock()
	defer c.m.Unlock()
	for _, t := range c.torrents {
		t.Close()
	}
	c.torrents = nil
	return c.db.Close()
}

func (c *Client) ListTorrents() []*Torrent {
	c.m.RLock()
	defer c.m.RUnlock()
	torrents := make([]*Torrent, 0, len(c.torrents))
	for _, t := range c.torrents {
		torrents = append(torrents, t)
	}
	return torrents
}

func (c *Client) AddTorrent(r io.Reader) (*Torrent, error) {
	return c.add(func(port int, sto storage.Storage) (*torrent.Torrent, error) {
		return torrent.New(r, port, sto, c.config.Torrent)
	})
}

func (c *Client) AddMagnet(link string) (*Torrent, error) {
	return c.add(func(port int, sto storage.Storage) (*torrent.Torrent, error) {
		return torrent.NewMagnet(link, port, sto, c.config.Torrent)
	})
}

func (c *Client) add(f func(port int, sto storage.Storage) (*torrent.Torrent, error)) (*Torrent, error) {
	var id uint64
	err := c.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(mainBucket)
		id, _ = bucket.NextSequence()
		return nil
	})
	if err != nil {
		return nil, err
	}

	dest := filepath.Join(c.config.DataDir, strconv.FormatUint(id, 10))
	sto, err := filestorage.New(dest)
	if err != nil {
		return nil, err
	}

	// TODO get port from config
	t, err := f(6881, sto)
	if err != nil {
		return nil, err
	}

	subBucket := strconv.FormatUint(id, 10)
	if err != nil {
		return nil, err
	}

	res, err := boltdbresumer.New(c.db, mainBucket, []byte(subBucket))
	if err != nil {
		return nil, err
	}
	t.SetResume(res)

	t.Start()

	t2 := &Torrent{
		ID:      id,
		Torrent: t,
	}

	c.m.Lock()
	defer c.m.Unlock()
	c.torrents[id] = t2
	return t2, nil
}

func (c *Client) GetTorrent(id uint64) *Torrent {
	c.m.RLock()
	defer c.m.RUnlock()
	return c.torrents[id]
}

func (c *Client) RemoveTorrent(id uint64) {
	c.m.Lock()
	defer c.m.Unlock()
	t, ok := c.torrents[id]
	if !ok {
		return
	}
	delete(c.torrents, id)
	t.Close()
}
