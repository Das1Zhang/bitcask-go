package bitcask_go

import (
	"bitcask-go/data"
	"bitcask-go/fio"
	"bitcask-go/index"
	"bitcask-go/utils"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gofrs/flock"
)

const (
	seqNoKey     = "seq.no"
	fileLockName = "flock"
)

// DB bitcask 存储引擎实例
type DB struct {
	options         Options                   // 用户配置
	mu              *sync.RWMutex             // 互斥锁
	fileIds         []int                     // 文件 id，只能在加载索引的时候使用，不能再其他地方更新喝使用
	activeFile      *data.DataFile            // 当前活跃文件，可以用于写入
	olderFiles      map[uint32]*data.DataFile // 旧数据文件，只可读
	index           index.Indexer             // 内存索引
	seqNo           uint64                    //事务序列号，全局递增
	isMerging       bool                      // 是否正在 merge
	seqNoFileExists bool                      // 存储事务序列号文件是否存在
	isInitial       bool                      // 是否第一次初始化此数据目录
	fileLock        *flock.Flock              // 文件锁保证多进程之间的互斥
	bytesWrite      uint                      // 累计写了多少个字节
	reclaimSize     int64                     // 表示有多少数据是无效的
}

// Stat 存储引擎统计信息
type Stat struct {
	KeyNum          uint  // key 的综述沥青
	DataFileNum     uint  // 磁盘数据文件总数量
	ReclaimableSize int64 // 可以进行 merge 回收的总数量，字节为单位
	DiskSize        int64 // 数据目录所占磁盘大小
}

// Open 打开 bitcask 存储引擎实例
func Open(options Options) (*DB, error) {
	// 对用户传入的配置项进行校验
	if err := checkOptions(options); err != nil {
		return nil, err
	}

	var isInitial bool
	// 对用户传入的目录进行校验，如果不存在则需要进行创建
	if _, err := os.Stat(options.DirPath); os.IsNotExist(err) {
		isInitial = true
		if err := os.Mkdir(options.DirPath, os.ModePerm); err != nil {
			return nil, err
		}
	}

	// 判断当前数据目录是否正在使用
	fileLock := flock.New(filepath.Join(options.DirPath, fileLockName))
	hold, err := fileLock.TryLock()
	if err != nil {
		return nil, err
	}
	if !hold {
		return nil, ErrDatabaseIsUsing
	}
	// 该目录确实存在，但是为空，这也算第一次加载数据目录
	entries, err := os.ReadDir(options.DirPath)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		isInitial = true
	}
	// 初始化 DB 实例结构体
	db := &DB{
		options:    options,
		mu:         new(sync.RWMutex),
		olderFiles: make(map[uint32]*data.DataFile),
		index:      index.NewIndexer(index.IndexType(options.IndexType), options.DirPath, options.SyncWrites),
		isInitial:  isInitial,
		fileLock:   fileLock,
	}
	// 加载 merge 数据目录
	if err := db.loadMergeFiles(); err != nil {
		return nil, err
	}

	// 加载数据文件
	if err := db.loadDataFile(); err != nil {
		return nil, err
	}

	// B+ 树不需要从数据文件中夹在索引
	if options.IndexType != BPlusTree {
		// 从 hint 索引文件中加载索引
		if err := db.loadIndexFromHintFile(); err != nil {
			return nil, err
		}

		// 从数据文件中加载索引
		if err := db.loadIndexFromDataFiles(); err != nil {
			return nil, err
		}

		// 重置 IO 类型为标准文件 IO（mmap只用于启动加速）
		if db.options.MMapAtStartUp {
			if err := db.resetIoType(); err != nil {
				return nil, err
			}
		}
	}
	// 取出当前事务序列号
	if options.IndexType == BPlusTree {
		if err := db.loadSeqNo(); err != nil {
			return nil, err
		}
		if db.activeFile != nil {
			size, err := db.activeFile.IoManager.Size()
			if err != nil {
				return nil, err
			}
			db.activeFile.WriteOff = size
		}
	}

	return db, nil
}

// Close 关闭数据库
func (db *DB) Close() error {
	defer func() {
		if err := db.fileLock.Unlock(); err != nil {
			panic(fmt.Sprintf("failed to unlock the directory, %v", err))
		}
	}()
	if db.activeFile == nil {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	// 关闭索引
	if err := db.index.Close(); err != nil {
		return err
	}

	// 保存当前事务序列号
	seqNoFile, err := data.OpenSeqNoFile(db.options.DirPath)
	if err != nil {
		return err
	}
	record := &data.LogRecord{
		Key:   []byte(seqNoKey),
		Value: []byte(strconv.FormatUint(db.seqNo, 10)),
	}
	encRecord, _ := data.EncodeLogRecord(record)
	if err := seqNoFile.Write(encRecord); err != nil {
		return err
	}
	if err := seqNoFile.Sync(); err != nil {
		return err
	}
	// 关闭当前活跃文件
	if err := db.activeFile.Close(); err != nil {
		return err
	}
	// 关闭旧的数据文件
	for _, file := range db.olderFiles {
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

// 持久化数据文件
func (db *DB) Sync() error {
	if db.activeFile == nil {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.activeFile.Sync()
}

// Stat 返回数据库的相关统计信息
func (db *DB) Stat() *Stat {
	db.mu.RLock()
	defer db.mu.RUnlock()

	var dataFiles = uint(len(db.olderFiles))
	if db.activeFile != nil {
		dataFiles += 1
	}
	dirSize, err := utils.DirSize(db.options.DirPath)
	if err != nil {
		panic(fmt.Sprintf("failed to get dir size, %v", err))
	}
	return &Stat{
		KeyNum:          uint(db.index.Size()),
		DataFileNum:     dataFiles,
		ReclaimableSize: db.reclaimSize,
		DiskSize:        dirSize,
	}
}

// Backup 备份数据库，将数据文件拷贝到新的目录中
func (db *DB) Backup(dir string) error {
	db.mu.RLock()
	defer db.mu.RUnlock()

	// 文件锁需要排除
	return utils.CopyDir(db.options.DirPath, dir, []string{fileLockName})
}

// 写入 Key/Value 数据，Key 不可为空
func (db *DB) Put(key []byte, value []byte) error {
	// 判断 key 是否有效
	if len(key) == 0 {
		return ErrKeyIsEmpty
	}

	// 构造 LogRecord 结构体
	log_record := &data.LogRecord{
		Key:   logRecordKeyWithSeq(key, nonTransactionSeqNo),
		Value: value,
		Type:  data.LogRecordNormal,
	}

	// 追加写入到当前的活跃文件中
	pos, err := db.appendLogRecordWithLock(log_record)
	if err != nil {
		return err
	}

	// 更新内存索引
	if oldPos := db.index.Put(key, pos); oldPos != nil {
		db.reclaimSize += int64(oldPos.Size)
	}

	return nil
}

// Delete 根据 Key 删除对应的数据
func (db *DB) Delete(key []byte) error {
	// 判断 key 的有效性
	if len(key) == 0 {
		return ErrKeyIsEmpty
	}

	// 查找 key 是否存在，如果不存在直接返回
	if pos := db.index.Get(key); pos == nil {
		return nil
	}

	// 构造 LogRecord 标识其是被删除的
	logRecord := &data.LogRecord{Key: logRecordKeyWithSeq(key, nonTransactionSeqNo), Type: data.LogRecordDeleted}

	pos, err := db.appendLogRecordWithLock(logRecord)
	if err != nil {
		return err
	}
	// 标识 delete 的这条数据本身也是可以被删除的
	db.reclaimSize += int64(pos.Size)
	// 从内存索引中将对应的 key 进行删除
	oldPos, ok := db.index.Delete(key)
	if !ok {
		return ErrIndexUpdateFailed
	}
	if oldPos != nil {
		db.reclaimSize += int64(oldPos.Size)
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

	// 从数据文件中获取 value
	return db.getValueByPosition(logRecordPos)
}

// 获取数据库中所有的 key
func (db *DB) ListKeys() [][]byte {
	iterator := db.index.Iterator(false)
	keys := make([][]byte, db.index.Size())
	var idx int
	for iterator.Rewind(); iterator.Valid(); iterator.Next() {
		keys[idx] = iterator.Key()
	}
	return keys
}

// Fold 获取所有的数据，并执行用户指定的操作，函数返回 false 则终止遍历
func (db *DB) Fold(fn func(key []byte, value []byte) bool) error {
	db.mu.RLock()
	defer db.mu.RUnlock()

	iterator := db.index.Iterator(false)
	defer iterator.Close()
	for iterator.Rewind(); iterator.Valid(); iterator.Next() {
		value, err := db.getValueByPosition(iterator.Value())
		if err != nil {
			return err
		}
		if !fn(iterator.Key(), value) {
			break
		}
	}
	return nil
}

// 根据索引信息获取对应的 value
func (db *DB) getValueByPosition(logRecordPos *data.LogRecordPos) ([]byte, error) {
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
	logRecord, _, err := dataFile.ReadLogRecord(logRecordPos.Offset)
	if err != nil {
		return nil, err
	}

	if logRecord.Type == data.LogRecordDeleted {
		return nil, ErrKeyNotFound
	}

	return logRecord.Value, nil
}

func (db *DB) appendLogRecordWithLock(logRecord *data.LogRecord) (*data.LogRecordPos, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.appendLogRecord(logRecord)
}

// 追加写入数据到活跃文件中
func (db *DB) appendLogRecord(logRecord *data.LogRecord) (*data.LogRecordPos, error) {

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

	db.bytesWrite += uint(size)
	// 根据用户配置决定是否持久化
	var needSync = db.options.SyncWrites
	if !needSync && db.options.BytesPerSync > 0 && db.bytesWrite >= db.options.BytesPerSync {
		needSync = true
	}
	if needSync {
		if err := db.activeFile.Sync(); err != nil {
			return nil, err
		}
		// 清空写入字节的累计值
		if db.bytesWrite > 0 {
			db.bytesWrite = 0
		}
	}

	// 构造内存索引信息
	pos := &data.LogRecordPos{Fid: db.activeFile.FileId, Offset: writeOff, Size: uint32(size)}
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

	// 打开新的数据文件
	dataFile, err := data.OpenDataFile(db.options.DirPath, initialFileId, fio.StandardFIO)
	if err != nil {
		return err
	}
	db.activeFile = dataFile
	return nil
}

func checkOptions(options Options) error {
	if options.DirPath == "" {
		return errors.New("database dir path is empty")
	}

	if options.DataFileSize <= 0 {
		return errors.New("database  data file size must be greater than 0")
	}

	if options.DataFileMergeRatio < 0 || options.DataFileMergeRatio > 1 {
		return errors.New("invalid merge ratio, must between 0 and 1")
	}
	return nil
}

func (db *DB) loadDataFile() error {
	dirEntries, err := os.ReadDir(db.options.DirPath)
	if err != nil {
		return err
	}
	var fileIds []int
	// 遍历所有数据文件，约定 .data 结尾即为数据文件
	for _, entry := range dirEntries {
		if strings.HasSuffix(entry.Name(), data.DataFileNameSuffix) {
			// 将文件进行分割，按照 "." 进行分割
			splitNames := strings.Split(entry.Name(), ".")
			fileId, err := strconv.Atoi(splitNames[0])
			// 文件目录有可能被损坏了
			if err != nil {
				return ErrDataDirectoryCorrupted
			}
			fileIds = append(fileIds, fileId)
		}
	}

	// 对文件 id 进行排序，从小到大依次进行加载
	sort.Ints(fileIds)
	db.fileIds = fileIds
	// 遍历每个文件 id，打开对应的数据文件
	for i, fid := range fileIds {
		// 如果采用了mmap，就使用mmap，否则用标准文件io
		ioType := fio.StandardFIO
		if db.options.MMapAtStartUp {
			ioType = fio.MemoryMap
		}
		dataFile, err := data.OpenDataFile(db.options.DirPath, uint32(fid), ioType)
		if err != nil {
			return err
		}
		// 最后一个 id 最大，说明是当前的活跃文件
		if i == len(fileIds)-1 {
			db.activeFile = dataFile
		} else {
			db.olderFiles[uint32(fid)] = dataFile
		}
	}
	return nil
}

func (db *DB) loadIndexFromDataFiles() error {
	// 空数据库，直接返回
	if len(db.fileIds) == 0 {
		return nil
	}

	// 查看是否发生过 merge
	hasMerge, nonMergeFileId := false, uint32(0)
	mergeFinFileName := filepath.Join(db.options.DirPath, data.MergeFinishedFileName)
	if _, err := os.Stat(mergeFinFileName); err == nil {
		fid, err := db.getNonMergeFileId(db.options.DirPath)
		if err != nil {
			return err
		}
		hasMerge = true
		nonMergeFileId = fid
	}
	// 构建内存索引并保存
	updateIndex := func(key []byte, typ data.LogRecordType, pos *data.LogRecordPos) {
		var oldPos *data.LogRecordPos
		if typ == data.LogRecordDeleted {
			oldPos, _ = db.index.Delete(key)
			db.reclaimSize += int64(oldPos.Size)
		} else {
			oldPos = db.index.Put(key, pos)
		}
		if oldPos != nil {
			db.reclaimSize += int64(oldPos.Size)
		}
	}

	// 暂存事务数据
	transactionRecords := make(map[uint64][]*data.TransactionRecord)
	var currentSeqNo = nonTransactionSeqNo
	// 遍历所有的文件id，处理文件中的记录
	for i, fid := range db.fileIds {
		var fileId = uint32(fid)
		// 如果比最近未参与merge 的文件 id 小，说明已经从 Hint 文件中加载过索引了，无需从数据文件中再加载一次
		if hasMerge && fileId < nonMergeFileId {
			continue
		}
		var dataFile *data.DataFile
		if fileId == db.activeFile.FileId {
			dataFile = db.activeFile
		} else {
			dataFile = db.olderFiles[fileId]
		}

		var offset int64 = 0
		for {
			logRecord, size, err := dataFile.ReadLogRecord(offset)
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			// 构造内存索引并保存
			logRecordPos := &data.LogRecordPos{Fid: fileId, Offset: offset, Size: uint32(size)}
			// 解析 key，拿到事务序列号
			realKey, seqNo := parseLogRecordKey(logRecord.Key)
			if seqNo == nonTransactionSeqNo {
				// 非事务操作，直接更新内存索引
				updateIndex(realKey, data.LogRecordDeleted, logRecordPos)
			} else {
				// 事务完成，对应的 seq no 的数据可以更新到内存中
				if logRecord.Type == data.LogRecordTxnFinished {
					for _, txnRecord := range transactionRecords[seqNo] {
						updateIndex(txnRecord.Record.Key, txnRecord.Record.Type, txnRecord.Pos)
					}
					delete(transactionRecords, seqNo)
				} else {
					logRecord.Key = realKey
					transactionRecords[seqNo] = append(transactionRecords[seqNo], &data.TransactionRecord{
						Record: logRecord,
						Pos:    logRecordPos,
					})
				}
			}

			// 更新事务序列号
			if seqNo > currentSeqNo {
				currentSeqNo = seqNo
			}
			// 递增 offset，从下一次新的位置开始读取
			offset += size
		}

		// 如果是当前活跃文件，更新这个文件的 WriteOff
		if i == len(db.fileIds)-1 {
			db.activeFile.WriteOff = offset
		}
	}
	// 更新事务序列号
	db.seqNo = currentSeqNo
	return nil
}

func (db *DB) loadSeqNo() error {
	fileName := filepath.Join(db.options.DirPath, data.SeqNoFileName)
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		return nil
	}

	seqNoFile, err := data.OpenSeqNoFile(db.options.DirPath)
	if err != nil {
		return err
	}
	record, _, err := seqNoFile.ReadLogRecord(0)
	seqNo, err := strconv.ParseUint(string(record.Value), 10, 64)
	if err != nil {
		return err
	}
	db.seqNo = seqNo
	db.seqNoFileExists = true
	return nil
}

// 将数据文件的 IO 重置为标准文件 IO
func (db *DB) resetIoType() error {
	if db.activeFile == nil {
		return nil
	}
	if err := db.activeFile.SetIOManager(db.options.DirPath, fio.StandardFIO); err != nil {
		return err
	}
	for _, dataFile := range db.olderFiles {
		if err := dataFile.SetIOManager(db.options.DirPath, fio.StandardFIO); err != nil {
			return err
		}
	}
	return nil
}
