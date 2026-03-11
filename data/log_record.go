package data

// LogRecordPos 内存索引，描述数据在文件上的位置
type LogRecordPos struct {
	Fid    uint32 // 文件id
	Offset int64  // 偏移，数据在文件中的位置
}
