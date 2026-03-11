package fio

const DataFilePerm = 0644

// IOManager 定义一个 IO 管理接口, 可以接入不同的IO类型
type IOManager interface {
	// Read 从文件的给定位置 读取对应数据
	Read([]byte, int64) (int, error)
	// Write 写入字节数组到文件中
	Write([]byte) (int, error)
	// Sync 将内存缓冲区中的数据持久化到磁盘当中
	Sync() error
	// Close 关闭文件
	Close() error
}
