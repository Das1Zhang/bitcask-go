package bitcask_go

import "os"

type Options struct {
	// 数据库的数据目录
	DirPath string
	// 数据文件的大小
	DataFileSize int64
	// 每次写入数据之后是否要进行持久化
	SyncWrites bool
	// 累计写到多少字节之后进行持久化
	BytesPerSync uint
	// 索引类型
	IndexType IndexerType

	//  启动时是否使用 mmap 加载
	MMapAtStartUp bool

	// 数据文件合并的阈值
	DataFileMergeRatio float32
}

// 索引迭代器配置项
type IteratorOptions struct {
	// 遍历前缀为指定值的 Key，默认为空
	Prefix []byte
	// 是否反向遍历，默认false 为正向
	Reverse bool
}

// WriteBatchOoptions 批量写配置项
type WriteBatchOptions struct {
	// 一个批次中最大的数据量
	MaxBatchNum uint

	// 提交事务时是否进行sync持久化
	SyncWrites bool
}

type IndexerType = int8

const (
	// BTree 索引
	Btree IndexerType = iota + 1
	// ART Adaptive Radix Tree 自适应基数树索引
	ART

	// BPlusTree B+ 树索引，将索引存储到磁盘上
	BPlusTree
)

var DefaultOptions = Options{
	DirPath:            os.TempDir(),
	DataFileSize:       256 * 1024 * 1024, // 256MB
	SyncWrites:         false,
	BytesPerSync:       0,
	IndexType:          Btree,
	MMapAtStartUp:      true,
	DataFileMergeRatio: 0.5,
}

var DefaultIteratorOptions = IteratorOptions{
	Prefix:  nil,
	Reverse: false,
}

var DefaultWriteBatchOptions = WriteBatchOptions{
	MaxBatchNum: 10000,
	SyncWrites:  true,
}
