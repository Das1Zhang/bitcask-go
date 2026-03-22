package bitcask_go

import (
	"bitcask-go/data"
	"bitcask-go/utils"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
)

const (
	mergeDirName     = "-merge"
	MergeFinishedKey = "merge.finished"
)

// Merge 清理无效数据，生成 hint 文件
func (db *DB) Merge() error {
	// 没有活跃文件，数据库为空，直接返回
	if db.activeFile == nil {
		return nil
	}
	db.mu.Lock()
	// 如果 merge 正在进行，则直接返回
	if db.isMerging {
		db.mu.Unlock()
		return ErrMergeIspProgress
	}

	// 查看可以 merge 的数据量是否达到了阈值
	totalSize, err := utils.DirSize(db.options.DirPath)
	if err != nil {
		db.mu.Unlock()
		return err
	}
	if float32(db.reclaimSize)/float32(totalSize) < db.options.DataFileMergeRatio {
		db.mu.Unlock()
		return ErrMergeRatioUnreached
	}

	// 查看剩余空间容量是否可以容纳 merge 之后的数据量
	availableDiskSize, err := utils.AvailableDiskSize()
	if err != nil {
		db.mu.Unlock()
		return err
	}
	if uint64(totalSize-db.reclaimSize) >= availableDiskSize {
		db.mu.Unlock()
		return ErrNoEnoughSpaceForMerge
	}
	// 开始执行 merge
	db.isMerging = true
	defer func() {
		db.isMerging = false
	}()

	// 持久化当前活跃文件
	if err := db.activeFile.Sync(); err != nil {
		return err
	}

	// 将当前活跃文件转化为一个旧的数据文件
	db.olderFiles[db.activeFile.FileId] = db.activeFile
	// 打开新的活跃文件
	if err := db.setActiveDataFile(); err != nil {
		db.mu.Unlock()
		return nil
	}
	// 记录最近没有参与 merge 的文件 id
	nonMergeFileId := db.activeFile.FileId

	// 取出所有需要 merge 的文件
	var mergeFiles []*data.DataFile
	for _, file := range db.olderFiles {
		mergeFiles = append(mergeFiles, file)
	}
	db.mu.Unlock() // 所有需要merge 的文件已暂存完毕，释放锁，可以接受用户新的写入，对需要merge 的文件不产生影响

	// 待 merge 的文件从小到大进行排序，依次 merge
	sort.Slice(mergeFiles, func(i, j int) bool {
		return mergeFiles[i].FileId < mergeFiles[j].FileId
	})

	mergePath := db.getMergePath()
	// 如果目录存在，说明发生过merge，将其删除
	if _, err := os.Stat(mergePath); err != nil {
		if err := os.RemoveAll(mergePath); err != nil {
			return err
		}
	}

	// 新建一个 merge path 的目录
	if err := os.MkdirAll(mergePath, os.ModePerm); err != nil {
		return err
	}

	// 打开一个临时的 bitcask 实例
	mergeOptions := db.options
	mergeOptions.DirPath = mergePath
	mergeOptions.SyncWrites = false // 自己控制sync，打开的话，merge可能会非常缓慢
	mergeDB, err := Open(mergeOptions)
	if err != nil {
		return err
	}

	// 打开 hint 文件存储索引
	hintFile, err := data.OpenHintFile(mergePath)
	if err != nil {
		return err
	}
	// 遍历处理每个数据文件
	for _, dataFile := range mergeFiles {
		var offset int64 = 0
		for {
			logRecord, size, err := dataFile.ReadLogRecord(offset)
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}

			// 解析拿到实际的 key
			realKey, _ := parseLogRecordKey(logRecord.Key)
			logRecordPos := db.index.Get(realKey)
			// 和内存中的索引位置进行比较，如果有效则重写
			if logRecordPos != nil &&
				logRecordPos.Fid == dataFile.FileId &&
				logRecordPos.Offset == offset {
				// 清除事务标记
				logRecord.Key = logRecordKeyWithSeq(realKey, nonTransactionSeqNo)
				pos, err := mergeDB.appendLogRecord(logRecord)
				if err != nil {
					return err
				}
				// 将当前位置索引写入到 Hind 文件中
				if err := hintFile.WriteHintRecord(realKey, pos); err != nil {
					return err
				}
			}
			// 处理完一个文件，递增offset
			offset += size
		}
	}

	// sync 保证持久化
	if err := hintFile.Sync(); err != nil {
		return err
	}

	if err := mergeDB.Sync(); err != nil {
		return err
	}

	// 新增标识merge完成的文件
	mergeFinishedFile, err := data.OpenMergeFinishedFile(mergePath)
	if err != nil {
		return err
	}
	mergeFinRecord := &data.LogRecord{
		Key:   []byte(MergeFinishedKey),
		Value: []byte(strconv.Itoa(int(nonMergeFileId))),
	}
	encRecord, _ := data.EncodeLogRecord(mergeFinRecord)
	if err := mergeFinishedFile.Write(encRecord); err != nil {
		return err
	}
	if err := mergeFinishedFile.Sync(); err != nil {
		return err
	}

	return nil
}

func (db *DB) getMergePath() string {
	// 拿到父级目录，并清除掉结尾的 '/'
	dir := path.Dir(path.Clean(db.options.DirPath))
	base := path.Base(db.options.DirPath)
	return filepath.Join(dir, base+mergeDirName)
}

// loadMergeFiles 加载 merge 数据目录
func (db *DB) loadMergeFiles() error {
	mergePath := db.getMergePath()
	// merge 目录不存在的话直接返回
	if _, err := os.Stat(mergePath); os.IsNotExist(err) {
		return nil
	}
	defer func() {
		_ = os.RemoveAll(mergePath)
	}()

	dirEntries, err := os.ReadDir(mergePath)
	if err != nil {
		return err
	}

	// 查找标识 merge 完成的文件，判断 merge 是否处理完毕
	var mergeFinished bool
	var mergeFileNames []string
	for _, entry := range dirEntries {
		if entry.Name() == data.MergeFinishedFileName {
			mergeFinished = true
		}
		if entry.Name() == data.SeqNoFileName {
			continue
		}
		if entry.Name() == fileLockName {
			continue
		}
		mergeFileNames = append(mergeFileNames, entry.Name())
	}

	// merge 没有完成直接返回
	if !mergeFinished {
		return nil
	}

	// merge 完成
	nonMergeFileId, err := db.getNonMergeFileId(mergePath)
	if err != nil {
		return nil
	}

	// 删除旧的数据文件
	var fileId uint32 = 0
	for ; fileId < nonMergeFileId; fileId++ {
		fileName := data.GetDataFileName(db.options.DirPath, fileId)
		if _, err := os.Stat(fileName); err == nil {
			if err := os.Remove(fileName); err != nil {
				return err
			}
		}
	}

	// 将新的数据文件移动到目录中
	for _, fileName := range mergeFileNames {
		srcPath := filepath.Join(mergePath, fileName)
		destPath := filepath.Join(db.options.DirPath, fileName)
		// 使用 os.Rename 移动文件
		if err := os.Rename(srcPath, destPath); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) getNonMergeFileId(mergePath string) (uint32, error) {
	mergeFinishedFile, err := data.OpenMergeFinishedFile(mergePath)
	if err != nil {
		return 0, err
	}
	record, _, err := mergeFinishedFile.ReadLogRecord(0)
	if err != nil {
		return 0, err
	}
	nonMergeFileId, err := strconv.Atoi(string(record.Value))
	if err != nil {
		return 0, err
	}
	return uint32(nonMergeFileId), nil
}

// 从 hint 文件中加载索引
func (db *DB) loadIndexFromHintFile() error {
	// 查看 hint 索引文件是否存在
	hintFileName := filepath.Join(db.options.DirPath, data.HintFileName)
	if _, err := os.Stat(hintFileName); os.IsNotExist(err) {
		return nil
	}

	// 打开 hint 索引文件
	hintFile, err := data.OpenHintFile(db.options.DirPath)
	if err != nil {
		return err
	}

	// 读取文件中的索引
	var offset int64 = 0
	for {
		logRecord, size, err := hintFile.ReadLogRecord(offset)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		// 解码拿到实际的位置索引
		pos := data.DecodeLogRecordPos(logRecord.Key)
		db.index.Put(logRecord.Key, pos)
		offset += size
	}
	return nil
}
