package bitcask_go

import (
	"bitcask-go/data"
	"bitcask-go/index"
	"sync"
)

// DB bitcask 存储引擎实例
type DB struct {
	options    Options                   // 用户配置
	mu         *sync.RWMutex             // 互斥锁
	activeFile *data.DataFile            // 当前活跃文件，可以用于写入
	olderFiles map[uint32]*data.DataFile // 旧数据文件，只可读
	index      index.Indexer             // 内存索引
}

// 写入 Key/Value 数据，Key 不可为空
func (db *DB) Put(key []byte, value []byte) error {
	// 判断 key 是否有效
	if len(key) == 0 {
		return ErrKeyIsEmpty
	}

	// 构造 LogRecord 结构体
	log_record := data.LogRecord{
		Key:   key,
		Value: value,
		Type:  data.LogRecordNormal,
	}

	// 追加写入到当前的活跃文件中
	pos, err := db.appendLogRecord(&log_record)
	if err != nil {
		return err
	}

	// 更新内存索引
	if ok := db.index.Put(key, pos); !ok {
		return ErrIndexUpdateFailed
	}

	return nil
}

// Get 根据 Key 来读取数据
func (db *DB) Get(key []byte) ([]byte, error) {
	// 读数据的时候进行一个锁的保护
	db.mu.RLock()
	defer db.mu.RUnlock()
	// 判断 key 的有效性
	if len(key) == 0 {
		return nil, ErrKeyIsEmpty
	}

	// 从内存数据结构中取出 key 对应的索引信息
	logRecordPos := db.index.Get(key)
	// 如果没找到索引信息，说明该 key 不存在
	if logRecordPos == nil {
		return nil, ErrKeyNotFound
	}

	// 根据文件 id 找到对应的数据文件
	var dataFile *data.DataFile
	if db.activeFile.FileId == logRecordPos.Fid {
		dataFile = db.activeFile
	} else {
		dataFile = db.olderFiles[logRecordPos.Fid]
	}

	// 如果数据文件为空
	if dataFile == nil {
		return nil, ErrDataFileNotFound
	}

	// 找到了对应的数据文件，根据偏移量读取数据
	logRecord, err := dataFile.ReadLogRecord(logRecordPos.Offset)
	if err != nil {
		return nil, err
	}

	if logRecord.Type == data.LogRecordDeleted {
		return nil, ErrKeyNotFound
	}

	return logRecord.Value, nil
}

// 追加写入数据到活跃文件中
func (db *DB) appendLogRecord(logRecord *data.LogRecord) (*data.LogRecordPos, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	// 判断当前活跃文件是否存在，如果不存在（为空），需要将其初始化
	// 数据库在没有写入的时候是没有文件生成的
	if db.activeFile == nil {
		if err := db.setActiveDataFile(); err != nil {
			return nil, err
		}
	}

	// 写入数据编码
	encRecord, size := data.EncodeLogRecord(logRecord)
	// 如果写入的数据已经到达了活跃文件的阈值，关闭活跃文件，并打开新的文件
	if db.activeFile.WriteOff+size > db.options.DataFileSize {
		// 将当前活跃文件先持久化到磁盘当中
		if err := db.activeFile.Sync(); err != nil {
			return nil, err
		}

		// 当前活跃文件转换为旧的数据文件，将当前的活跃文件放入旧数据文件的map中
		db.olderFiles[db.activeFile.FileId] = db.activeFile

		// 打开新的数据文件，将其设置为活跃文件
		if err := db.setActiveDataFile(); err != nil {
			return nil, err
		}
	}

	writeOff := db.activeFile.WriteOff
	if err := db.activeFile.Write(encRecord); err != nil {
		return nil, err
	}

	// 根据用户配置决定是否持久化
	if db.options.SyncWrites {
		if err := db.activeFile.Sync(); err != nil {
			return nil, err
		}
	}

	// 构造内存索引信息
	pos := &data.LogRecordPos{Fid: db.activeFile.FileId, Offset: writeOff}
	return pos, nil
}

// 设置当前活跃文件
// 在访问此方法前必须持有互斥锁
func (db *DB) setActiveDataFile() error {
	var initialFileId uint32 = 0
	// 当前活跃文件不为空，新的活跃文件ID需要递增
	if db.activeFile != nil {
		initialFileId = db.activeFile.FileId + 1
	}

	//
	dataFile, err := data.OpenDataFile(db.options.DirPath, initialFileId)
	if err != nil {
		return err
	}
	db.activeFile = dataFile
	return nil
}
