package data

type LogRecordType = byte

const (
	LogRecordNormal LogRecordType = iota
	LogRecordDeleted
)

// 写入到数据文件的记录
// 数据文件中的数据是追加写入的，所以是日志的格式，叫做日志
type LogRecord struct {
	Key   []byte
	Value []byte
	Type  LogRecordType
}

// LogRecordPos 内存索引，描述数据在文件上的位置
type LogRecordPos struct {
	Fid    uint32 // 文件id
	Offset int64  // 偏移，数据在文件中的位置
}

// EncodeLogRecord 对 LogRecord 进行编码，返回字节数组和长度
func EncodeLogRecord(logRecord *LogRecord) ([]byte, int64) {
	return nil, 0
}
