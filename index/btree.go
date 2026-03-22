package index

import (
	"bitcask-go/data"
	"bytes"
	"sort"
	"sync"

	"github.com/google/btree"
)

// BTree 索引，封装 google 的 btree kv
// https://github.com/google.btree
type BTree struct {
	tree *btree.BTree
	lock *sync.RWMutex
}

// NewBTree 初始化 BTree 索引结构
func NewBTree() *BTree {
	return &BTree{
		tree: btree.New(32), // btree的叶子结点数量
		lock: new(sync.RWMutex),
	}
}

func (bt *BTree) Put(key []byte, pos *data.LogRecordPos) *data.LogRecordPos {
	it := &Item{key: key, pos: pos}
	bt.lock.Lock()
	oldItem := bt.tree.ReplaceOrInsert(it)
	bt.lock.Unlock()
	if oldItem == nil {
		return nil
	}
	return oldItem.(*Item).pos
}

func (bt *BTree) Get(key []byte) *data.LogRecordPos {
	it := &Item{key: key}
	btreeItem := bt.tree.Get(it)
	if btreeItem == nil {
		return nil
	}
	return btreeItem.(*Item).pos
}

func (bt *BTree) Delete(key []byte) (*data.LogRecordPos, bool) {
	it := &Item{key: key}
	bt.lock.Lock()
	oldItem := bt.tree.Delete(it)
	bt.lock.Unlock()
	if oldItem == nil {
		return nil, false
	}
	return oldItem.(*Item).pos, true
}

func (bt *BTree) Size() int {
	return bt.tree.Len()
}

func (bt *BTree) Iterator(reverse bool) Iterator {
	if bt.tree == nil {
		return nil
	}
	bt.lock.RLock()
	defer bt.lock.RUnlock()
	return newBTreeIterator(bt.tree, reverse)
}

func (bt *BTree) Close() error {
	return nil
}

// BTree 索引迭代器
type btreeIterator struct {
	currIndex int     // 当前遍历到的位置，数组的下标
	reverse   bool    // 是否是反向遍历
	values    []*Item // key + 位置索引信息
}

func newBTreeIterator(tree *btree.BTree, reverse bool) *btreeIterator {
	var idx int
	values := make([]*Item, tree.Len())

	// 将所有的数据存放到数组中，如果返回false会终止遍历
	saveValues := func(it btree.Item) bool {
		values[idx] = it.(*Item)
		idx++
		return true
	}

	if reverse {
		tree.Descend(saveValues)
	} else {
		tree.Ascend(saveValues)
	}

	return &btreeIterator{
		currIndex: 0,
		reverse:   reverse,
		values:    values,
	}
}

func (bti *btreeIterator) Rewind() {
	bti.currIndex = 0
}

func (bti *btreeIterator) Seek(key []byte) {
	if bti.reverse {
		bti.currIndex = sort.Search(len(bti.values), func(i int) bool {
			return bytes.Compare(bti.values[i].key, key) <= 0
		})
	} else {
		bti.currIndex = sort.Search(len(bti.values), func(i int) bool {
			return bytes.Compare(bti.values[i].key, key) >= 0
		})
	}

}

// Next 跳转到下一个 key
func (bti *btreeIterator) Next() {
	bti.currIndex += 1
}

// Valid 是否有效，即是否已经遍历完了所有的 key，用于退出遍历
func (bti *btreeIterator) Valid() bool {
	return bti.currIndex < len(bti.values)
}

// Key 当前遍历位置的 key 数据
func (bti *btreeIterator) Key() []byte {
	return bti.values[bti.currIndex].key
}

// Value() 当前遍历位置的 value 数据
func (bti *btreeIterator) Value() *data.LogRecordPos {
	return bti.values[bti.currIndex].pos
}

// Close 关闭迭代器，释放相应资源
func (bti *btreeIterator) Close() {
	bti.values = nil
}
