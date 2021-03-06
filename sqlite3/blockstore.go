package sqlite3bs

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/mattn/go-sqlite3"
)

// pragmas are sqlite pragmas to be applied at initialization.
var pragmas = []string{
	fmt.Sprintf("PRAGMA busy_timeout = %d", 10*1000), // milliseconds
	"PRAGMA synchronous = OFF",
	"PRAGMA temp_store = memory",
	// "PRAGMA mmap_size = 268435456",
	"PRAGMA cache_size = -524288",
	"PRAGMA page_size = 4096",
	"PRAGMA auto_vacuum = NONE",
	"PRAGMA automatic_index = OFF",
	"PRAGMA journal_mode = memory",
	// "PRAGMA read_uncommitted = ON",
}

var initDDL = []string{
	// spacing not important
	`CREATE TABLE IF NOT EXISTS blocks (
		mh TEXT NOT NULL PRIMARY KEY,
		bytes BLOB NOT NULL
	) WITHOUT ROWID`,

	// placeholder version to enable migrations.
	`CREATE TABLE IF NOT EXISTS _meta (
    	version UINT64 NOT NULL UNIQUE
	)`,

	// version 1.
	`INSERT OR IGNORE INTO _meta (version) VALUES (1)`,
}

const (
	stmtHas = iota
	stmtGet
	stmtGetSize
	stmtPut
	stmtDelete
	stmtSelectAll
)

// statements are statements to prepare.
var statements = [...]string{
	stmtHas:       "SELECT EXISTS (SELECT 1 FROM blocks WHERE mh = ?)",
	stmtGet:       "SELECT bytes FROM blocks WHERE mh = ?",
	stmtGetSize:   "SELECT LENGTH(bytes) FROM blocks WHERE mh = ?",
	stmtPut:       "INSERT OR IGNORE INTO blocks (mh, bytes) VALUES (?, ?)",
	stmtDelete:    "DELETE FROM blocks WHERE mh = ?",
	stmtSelectAll: "SELECT mh FROM blocks",
}

// Blockstore is a sqlite backed IPLD blockstore, highly optimized and
// customized for IPLD query and write patterns.
type Blockstore struct {
	lk sync.RWMutex
	db *sql.DB

	prepared [len(statements)]*sql.Stmt
}

var _ blockstore.Blockstore = (*Blockstore)(nil)

type Options struct {
	// placeholder
}

// counter of sqlite drivers registered; guarded by atomic.
var counter int64

// Open creates a new sqlite3-backed blockstore.
func Open(path string, _ Options) (*Blockstore, error) {
	driver := fmt.Sprintf("sqlite3_blockstore_%d", atomic.AddInt64(&counter, 1))
	sql.Register(driver,
		&sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				// Execute pragmas on connection creation.
				for _, p := range pragmas {
					if _, err := conn.Exec(p, nil); err != nil {
						return fmt.Errorf("failed to execute sqlite3 init pragma: %s; err: %w", p, err)
					}
				}
				return nil
			},
		})

	db, err := sql.Open(driver, path+"?mode=rwc")
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite3 database: %w", err)
	}

	// Execute init DDLs.
	for _, ddl := range initDDL {
		if _, err := db.Exec(ddl); err != nil {
			return nil, fmt.Errorf("failed to execute sqlite3 init DDL: %s; err: %w", ddl, err)
		}
	}

	bs := &Blockstore{db: db}

	// Prepare all statements.
	for i, p := range statements {
		if bs.prepared[i], err = db.Prepare(p); err != nil {
			return nil, fmt.Errorf("failed to prepare statement: %s; err: %w", p, err)
		}
	}
	return bs, nil
}

func (b *Blockstore) Has(cid cid.Cid) (bool, error) {
	var ret bool
	err := b.prepared[stmtHas].QueryRow(keyFromCid(cid)).Scan(&ret)
	if err != nil {
		err = fmt.Errorf("failed to check for existence of CID %s in sqlite3 blockstore: %w", cid, err)
	}
	return ret, err
}

func (b *Blockstore) Get(cid cid.Cid) (blocks.Block, error) {
	var data []byte
	switch err := b.prepared[stmtGet].QueryRow(keyFromCid(cid)).Scan(&data); err {
	case sql.ErrNoRows:
		return nil, blockstore.ErrNotFound
	case nil:
		return blocks.NewBlockWithCid(data, cid)
	default:
		return nil, fmt.Errorf("failed to get CID %s from sqlite3 blockstore: %w", cid, err)
	}
}

func (b *Blockstore) GetSize(cid cid.Cid) (int, error) {
	var size int
	switch err := b.prepared[stmtGetSize].QueryRow(keyFromCid(cid)).Scan(&size); err {
	case sql.ErrNoRows:
		// https://github.com/ipfs/go-ipfs-blockstore/blob/v1.0.1/blockstore.go#L183-L185
		return -1, blockstore.ErrNotFound
	case nil:
		return size, nil
	default:
		return -1, fmt.Errorf("failed to get size of CID %s from sqlite3 blockstore: %w", cid, err)
	}
}

func (b *Blockstore) Put(block blocks.Block) error {
	var (
		cid  = block.Cid()
		data = block.RawData()
	)

	_, err := b.prepared[stmtPut].Exec(keyFromCid(cid), data)
	if err != nil {
		err = fmt.Errorf("failed to put block with CID %s into sqlite3 blockstore: %w", cid, err)
	}
	return err
}

func (b *Blockstore) PutMany(blocks []blocks.Block) error {
	for i, blk := range blocks {
		if err := b.Put(blk); err != nil {
			return fmt.Errorf("failed to put block %d/%d with CID %s into sqlite3 blockstore: %w", i, len(blocks), blk.Cid(), err)
		}
	}
	return nil
}

func (b *Blockstore) DeleteBlock(cid cid.Cid) error {
	_, err := b.prepared[stmtDelete].Exec(keyFromCid(cid))
	return err
}

func (b *Blockstore) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	ret := make(chan cid.Cid)

	q, err := b.prepared[stmtSelectAll].QueryContext(ctx)
	if err == sql.ErrNoRows {
		close(ret)
		return ret, nil
	} else if err != nil {
		close(ret)
		return nil, fmt.Errorf("failed to query all keys from sqlite3 blockstore: %w", err)
	}

	go func() {
		defer close(ret)

		for q.Next() {
			var mh string

			switch err := q.Scan(&mh); {
			case err == nil:
				if mh, err := base64.RawStdEncoding.DecodeString(mh); err != nil {
					log.Printf("failed to parse multihash when querying all keys in sqlite3 blockstore: %s", err)
				} else {
					ret <- cid.NewCidV1(cid.Raw, mh)
				}
			case ctx.Err() != nil:
				return // context was cancelled
			default:
				log.Printf("failed when querying all keys in sqlite3 blockstore: %s", err)
				return
			}
		}
	}()
	return ret, nil
}

func (b *Blockstore) HashOnRead(_ bool) {
	log.Print("sqlite3 blockstore ignored HashOnRead request")
}

func (b *Blockstore) Close() error {
	return b.db.Close()
}

func keyFromCid(c cid.Cid) string {
	return base64.RawStdEncoding.EncodeToString(c.Hash())
}
