package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	logging "log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	badger10 "gx/ipfs/QmQBccCGkYxLSdqzvUc6eTDqT9dqPcT7fCHzH6Z4ftWst3/badger"
	errors "gx/ipfs/QmVmDhyTTUcQXFD1rRQ64fGLMSAoaQvNH3hwuaCFAPq2hy/errors"
	badger08 "gx/ipfs/QmaYHhxyszcAYob7WP8nSXnkJjzwfsWyApZEJFaJoJnXNP/badger"
)

var Log = logging.New(os.Stderr, "upgrade ", logging.LstdFlags)
var ErrInvalidVersion = errors.New("unsupported badger version")
var ErrCancelled = errors.New("context cancelled")

const (
	LockFile   = "repo.lock"
	ConfigFile = "config"
	SpecsFile  = "datastore_spec"

	SuppertedRepoVersion = 6
)

type keyValue struct {
	key   []byte
	value []byte
}

type Process struct {
	path string

	ctx    context.Context
	cancel context.CancelFunc

	dbPaths map[string]struct{}
}

func Upgrade(baseDir string) error {
	ctx, cancel := context.WithCancel(context.Background())
	p := Process{
		path: baseDir,

		ctx:    ctx,
		cancel: cancel,

		dbPaths: map[string]struct{}{},
	}

	err := p.checkRepoVersion()
	if err != nil {
		return err
	}

	paths, err := p.loadSpecs()
	if err != nil {
		return err
	}

	for _, dir := range paths {
		err := p.upgradeDs(path.Join(p.path, dir))
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Process) upgradeDs(path string) error {
	Log.Printf("Upgrading badger at %s\n", path)

	Log.Printf("Trying badger 1.0\n")
	err := c.try10(path)
	if err == nil || err != ErrInvalidVersion {
		return err
	}

	Log.Printf("Trying badger 0.8\n")
	err = c.try08(path)
	if err == nil || err != ErrInvalidVersion {
		return err
	}

	return ErrInvalidVersion
}

func (c *Process) try10(path string) error {
	opt := badger10.DefaultOptions
	opt.Dir = path
	opt.ValueDir = path
	opt.SyncWrites = true

	db, err := badger10.Open(opt)
	if err != nil {
		if strings.HasPrefix(err.Error(), "manifest has unsupported version:") {
			err = ErrInvalidVersion
		}
		return err
	}

	db.Close()
	return nil
}

func (c *Process) try08(path string) error {
	opt := badger08.DefaultOptions
	opt.Dir = path
	opt.ValueDir = path
	opt.SyncWrites = true

	kv, err := badger08.NewKV(&opt)
	if err != nil {
		if strings.HasPrefix(err.Error(), "manifest has unsupported version:") {
			err = ErrInvalidVersion
		}
		return err
	}
	out := make(chan keyValue)
	go func() {
		defer kv.Close()
		it := kv.NewIterator(badger08.DefaultIteratorOptions)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			err := item.Value(func(data []byte) error {
				select {
				case out <- keyValue{key: item.Key(), value: data}:
				case <-c.ctx.Done():
					return ErrCancelled
				}
				return nil
			})
			if err == ErrCancelled {
				return
			}
			if err != nil {
				Log.Printf("Error: %s\n", err.Error())
				return
			}
		}
		close(out)
	}()

	return c.migrateData(out, path)
}

func (c *Process) migrateData(data chan keyValue, path string) error {
	temp, err := ioutil.TempDir(c.path, "badger-")
	if err != nil {
		c.cancel()
		return err
	}

	err = func() error {
		opt := badger10.DefaultOptions
		opt.ValueDir = temp
		opt.Dir = temp
		opt.SyncWrites = true
		db, err := badger10.Open(opt)
		if err != nil {
			c.cancel()
			return err
		}
		defer db.Close()

		txn := db.NewTransaction(true)
		defer txn.Discard()

		Log.Printf("Moving data to %s\n", temp)
		n := 0

		for entry := range data {
			err := txn.Set(entry.key, entry.value)
			if err != nil {
				c.cancel()
				return err
			}

			if n%1000 == 0 {
				Log.Printf("%d entries done\r\x1b[A", n)
			}
			n++
		}
		Log.Printf("%d entries done\n", n)
		Log.Printf("Commiting transaction\n")

		return txn.Commit(nil)
	}()
	if err != nil {
		return err
	}

	backup, err := ioutil.TempDir(c.path, "badger-backup-")
	if err != nil {
		return err
	}
	if err = os.Remove(backup); err != nil {
		return err
	}

	Log.Printf("Renaming '%s' to '%s'\n", path, backup)

	if err = os.Rename(path, backup); err != nil {
		return err
	}
	Log.Printf("Renaming '%s' to '%s'\n", temp, path)

	if err = os.Rename(temp, path); err != nil {
		return err
	}

	Log.Printf("Success\n")
	Log.Printf("vvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvvv")
	Log.Printf("AFTER YOU VERIFY THAT YOUR DATASTORE IS WORKING")
	Log.Printf("REMOVE '%s'", backup)
	Log.Printf("^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^")

	return nil
}

func (c *Process) loadSpecs() ([]string, error) {
	specData, err := ioutil.ReadFile(path.Join(c.path, SpecsFile))
	if err != nil {
		return nil, err
	}

	var spec map[string]interface{}
	err = json.Unmarshal(specData, &spec)
	if err != nil {
		return nil, err
	}

	return parseSpecs(spec)
}

func parseSpecs(spec map[string]interface{}) ([]string, error) {
	t, ok := spec["type"].(string)
	if !ok {
		return nil, errors.New("unexpected spec type")
	}

	switch t {
	case "mount":
		mounts, ok := spec["mounts"].([]interface{})
		if !ok {
			return nil, errors.New("unexpected mounts type")
		}

		var out []string

		for _, m := range mounts {
			mount, ok := m.(map[string]interface{})
			if !ok {
				return nil, errors.New("unexpected mount type")
			}

			paths, err := parseSpecs(mount)
			if err != nil {
				return nil, err
			}
			out = append(out, paths...)
		}
		return out, nil
	case "measure":
		child, ok := spec["child"].(map[string]interface{})
		if !ok {
			return nil, errors.New("unexpected child type")
		}

		return parseSpecs(child)
	case "badgerds":
		path, ok := spec["path"].(string)
		if !ok {
			return nil, errors.New("unexpected path type")
		}

		Log.Printf("Badger instance at %s\n", path)

		return []string{path}, nil
	case "flatfs", "levelds":
		return nil, nil
	default:
		return nil, errors.New("unexpected ds type")
	}
}

func (c *Process) checkRepoVersion() error {
	vstr, err := ioutil.ReadFile(filepath.Join(c.path, "version"))
	if err != nil {
		return err
	}

	version, err := strconv.Atoi(strings.TrimSpace(string(vstr)))
	if err != nil {
		return err
	}

	if version != SuppertedRepoVersion {
		return fmt.Errorf("unsupported fsrepo version: %d", version)
	}

	return nil
}
