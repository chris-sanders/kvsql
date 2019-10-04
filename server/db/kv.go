package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
)

var (
	ErrExists        = fmt.Errorf("key exists")
	ErrNotExists     = fmt.Errorf("key and or Revision does not exists")
	ErrRevisionMatch = fmt.Errorf("revision does not match")
)

type KeyValue struct {
	ID             int64
	Key            string
	Value          []byte
	OldValue       []byte
	OldRevision    int64
	CreateRevision int64
	Revision       int64
	TTL            int64
	Version        int64
	Del            int64
}

var (
	fieldList = "name, value, old_value, old_revision, create_revision, revision, ttl, version, del"
	baseList  = `
SELECT kv.id, kv.name, kv.value, kv.old_value, kv.old_revision, kv.create_revision, kv.revision, kv.ttl, kv.version, kv.del
FROM key_value kv
  INNER JOIN
    (
      SELECT MAX(revision) revision, kvi.name
      FROM key_value kvi
		%REV%
        GROUP BY kvi.name
    ) AS r
    ON r.name = kv.name AND r.revision = kv.revision
WHERE kv.name like ? %RES% ORDER BY kv.name ASC limit ?
`
	cleanupSQL      = "DELETE FROM key_value WHERE ttl > 0 AND ttl < ?"
	getSQL          = "SELECT id, " + fieldList + " FROM key_value WHERE name = ? ORDER BY revision DESC limit ?"
	listSQL         = strings.Replace(strings.Replace(baseList, "%REV%", "", -1), "%RES%", "", -1)
	listRevisionSQL = strings.Replace(strings.Replace(baseList, "%REV%", "WHERE kvi.revision >= ?", -1), "%RES%", "", -1)
	listResumeSQL   = strings.Replace(strings.Replace(baseList, "%REV%", "WHERE kvi.revision <= ?", -1),
		"%RES%", "and kv.name > ? ", -1)
	insertSQL = `
INSERT INTO key_value(` + fieldList + `)
   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	replaySQL    = "SELECT id, " + fieldList + " FROM key_value WHERE name like ? and revision >= ? ORDER BY revision ASC"
	toDeleteSQL  = "SELECT count(*) c, name, max(revision) FROM key_value GROUP BY name HAVING c > 1 or (c = 1 and del = 1)"
	deleteOldSQL = "DELETE FROM key_value WHERE name = ? AND (revision < ? OR (revision = ? AND del = 1))"
)

func (d *DB) List(ctx context.Context, revision, limit int64, rangeKey, startKey string) ([]*KeyValue, int64, error) {
	if limit == 0 {
		limit = 1000000
	} else {
		limit = limit + 1
	}

	var resp []*KeyValue
	var listRevision int64
	var err error

	err = d.tx(func(tx *sql.Tx) error {
		listRevision, err = currentRevision(ctx, tx)
		if err != nil {
			return err
		}
		query := ""
		args := []interface{}{}
		if !strings.HasSuffix(rangeKey, "%") && revision <= 0 {
			query = getSQL
			args = append(args, rangeKey, 1)
		} else if revision <= 0 {
			query = listSQL
			args = append(args, rangeKey, limit)
		} else if len(startKey) > 0 {
			listRevision = revision
			query = listResumeSQL
			args = append(args, revision, rangeKey, startKey, limit)
		} else {
			query = listRevisionSQL
			args = append(args, revision, rangeKey, limit)
		}

		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			value := KeyValue{}
			if err := scan(rows.Scan, &value); err != nil {
				return err
			}
			if value.Revision > listRevision {
				listRevision = value.Revision
			}
			if value.Del == 0 {
				resp = append(resp, &value)
			}
		}

		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	return resp, listRevision, nil
}

func (d *DB) Get(ctx context.Context, key string) (*KeyValue, error) {
	kvs, _, err := d.List(ctx, 0, 1, key, "")
	if err != nil {
		return nil, err
	}
	if len(kvs) > 0 {
		return kvs[0], nil
	}
	return nil, nil
}

func (d *DB) Update(ctx context.Context, key string, value []byte, revision, ttl int64) (*KeyValue, *KeyValue, error) {
	kv, err := d.mod(ctx, false, key, value, revision, ttl)
	if err != nil {
		return nil, nil, err
	}

	if kv.Version == 1 {
		return nil, kv, nil
	}

	oldKv := *kv
	oldKv.Revision = oldKv.OldRevision
	oldKv.Value = oldKv.OldValue
	return &oldKv, kv, nil
}

func (d *DB) Delete(ctx context.Context, key string, revision int64) ([]*KeyValue, error) {
	if strings.HasSuffix(key, "%") {
		panic("can not delete list revision")
	}

	_, err := d.mod(ctx, true, key, []byte{}, revision, 0)
	return nil, err
}

func (d *DB) mod(ctx context.Context, delete bool, key string, value []byte, revision int64, ttl int64) (*KeyValue, error) {
	oldKv, err := d.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	if revision > 0 && oldKv == nil {
		return nil, ErrNotExists
	}

	if revision > 0 && oldKv.Revision != revision {
		return nil, ErrRevisionMatch
	}

	if ttl > 0 {
		ttl = int64(time.Now().Unix()) + ttl
	}

	var result *KeyValue
	err = d.tx(func(tx *sql.Tx) error {
		newRevision, err := newRevision(ctx, tx)
		if err != nil {
			return err
		}
		result = &KeyValue{
			Key:            key,
			Value:          value,
			Revision:       newRevision,
			TTL:            int64(ttl),
			CreateRevision: newRevision,
			Version:        1,
		}
		if oldKv != nil {
			result.OldRevision = oldKv.Revision
			result.OldValue = oldKv.Value
			result.TTL = oldKv.TTL
			result.CreateRevision = oldKv.CreateRevision
			result.Version = oldKv.Version + 1
		}

		if delete {
			result.Del = 1
		}
		_, err = tx.ExecContext(ctx, insertSQL,
			result.Key,
			result.Value,
			result.OldValue,
			result.OldRevision,
			result.CreateRevision,
			result.Revision,
			result.TTL,
			result.Version,
			result.Del,
		)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if d.notify != nil {
		if err := d.notify(result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func (d *DB) Cleanup(ctx context.Context) error {
	return d.tx(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, toDeleteSQL)
		if err != nil {
			return err
		}
		defer rows.Close()

		toDelete := map[string]int64{}
		for rows.Next() {
			var (
				count, revision int64
				name            string
			)
			err := rows.Scan(&count, &name, &revision)
			if err != nil {
				return err
			}
			toDelete[name] = revision
		}

		for name, rev := range toDelete {
			_, err = tx.ExecContext(ctx, deleteOldSQL, name, rev, rev)
			if err != nil {
				return err
			}
		}

		return nil
	})

	return nil
}

func (d *DB) Replay(ctx context.Context, key string, revision int64) ([]*KeyValue, error) {
	var resp []*KeyValue
	err := d.tx(func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, replaySQL, key, revision)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			value := KeyValue{}
			if err := scan(rows.Scan, &value); err != nil {
				return err
			}
			resp = append(resp, &value)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func currentRevision(ctx context.Context, tx *sql.Tx) (int64, error) {
	row := tx.QueryRowContext(ctx, "SELECT id FROM revision")
	rev := sql.NullInt64{}
	if err := row.Scan(&rev); err != nil {
		return 0, errors.Wrap(err, "get current revision")
	}
	if rev.Int64 == 0 {
		panic("current revision is 0")
	}
	return rev.Int64, nil
}

func newRevision(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx, "DELETE FROM revision"); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, "INSERT INTO revision(t) VALUES(NULL)")
	if err != nil {
		return 0, err
	}
	revision, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	return revision, nil
}

type scanner func(dest ...interface{}) error

func scan(s scanner, out *KeyValue) error {
	return s(
		&out.ID,
		&out.Key,
		&out.Value,
		&out.OldValue,
		&out.OldRevision,
		&out.CreateRevision,
		&out.Revision,
		&out.TTL,
		&out.Version,
		&out.Del)
}
