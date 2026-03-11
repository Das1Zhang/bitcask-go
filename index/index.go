package index

import (
	"bitcask-go/data"
	"bytes"

	"github.com/google/btree"
)

type Indexer interface {
	// Put 向索引中存储 key 对应的位置信息
	Put(key []byte, pos *data.LogRecordPos) bool
	// Get 从索引中取出 key 对应的索引位置信息
	Get(key []byte) *data.LogRecordPos
	// Delete 根据 key 删除对应的索引位置信息
	Delete(key []byte) bool
}

type Item struct {
	key []byte
	pos *data.LogRecordPos
}

func (ai *Item) Less(bi btree.Item) bool {
	return bytes.Compare(ai.key, bi.(*Item).key) == -1
}
