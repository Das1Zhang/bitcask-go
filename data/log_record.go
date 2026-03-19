package data

import (
	"encoding/binary"
	"hash/crc32"
)

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

// crc, type, keySize, valueSize
// 4 +   1 +   5     +   5    =  15
const maxLogRecordHeaderSize = binary.MaxVarintLen32*2 + 5

// LogRecordPos 内存索引，描述数据在文件上的位置
type LogRecordPos struct {
	Fid    uint32 // 文件id
	Offset int64  // 偏移，数据在文件中的位置
}

// LogRecord 的头部信息
type LogRecordHeader struct {
	crc        uint32        // crc 校验值
	recordType LogRecordType // 标识 LogRecord 类型
	keySize    uint32        // key 的长度
	valueSize  uint32        // value 的长度
}

// EncodeLogRecord  zed对 LogRecord 进行编码，返回字节数组和长度
// crc 校验值 ｜ type 类型 ｜ key size ｜ value size ｜ key ｜ value
//
//	4          1       变长（最大5） 变长（最大5）  变长  变长
func EncodeLogRecord(logRecord *LogRecord) ([]byte, int64) {
	// 初始化一个 header 部分的字节数组
	header := make([]byte, maxLogRecordHeaderSize)

	// 从第五个字节开始存储 type 先跳过 crc
	header[4] = logRecord.Type
	var index = 5
	// 5 字节之后，存储的是 key 和 value 的长度信息
	// 使用 变长类型，节省空间
	index += binary.PutVarint(header[index:], int64(len(logRecord.Key)))
	index += binary.PutVarint(header[index:], int64(len(logRecord.Value)))

	var size = index + len(logRecord.Key) + len(logRecord.Value)
	encBytes := make([]byte, size)

	// 将 header 部分的内容拷贝过来
	copy(encBytes[:index], header[:index])
	// 将 key 和 value 数据拷贝到字节数组中
	copy(encBytes[index:], logRecord.Key)
	copy(encBytes[index+len(logRecord.Key):], logRecord.Value)

	// 对整个 LogRecord 的数据进行 crc 校验
	crc := crc32.ChecksumIEEE(encBytes[4:])
	// crc小端序
	binary.LittleEndian.PutUint32(encBytes[:4], crc)

	return encBytes, int64(size)
}

// 对字节数组中的 Header 进行解码得到 LogRecordHeader
func decodeLogRecordHeader(buf []byte) (*LogRecordHeader, int64) {
	if len(buf) < 4 {
		return nil, 0
	}

	header := &LogRecordHeader{
		crc:        binary.LittleEndian.Uint32(buf[:4]),
		recordType: buf[4],
	}
	var index = 5
	// 取出实际的 key size
	keySize, n := binary.Varint(buf[index:])
	header.keySize = uint32(keySize)
	index += n

	// 取出实际的 value size
	valueSize, n := binary.Varint(buf[index:])
	header.valueSize = uint32(valueSize)
	index += n

	return header, int64(index)
}

func getLogRecordCRC(lr *LogRecord, header []byte) uint32 {
	if lr == nil {
		return 0
	}
	crc := crc32.ChecksumIEEE(header[:])
	crc = crc32.Update(crc, crc32.IEEETable, lr.Key)
	crc = crc32.Update(crc, crc32.IEEETable, lr.Value)
	return crc
}
