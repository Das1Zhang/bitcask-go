package bitcask_go

type Options struct {
	// 数据库的数据目录
	DirPath string
	// 数据文件的大小
	DataFileSize int64
	// 每次写入数据之后是否要进行持久化
	SyncWrites bool
}
