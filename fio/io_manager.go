package fio

const DataFilePerm = 0644

type FileIOType = byte

const (
	// StandardFIO 标准文件IO
	StandardFIO FileIOType = iota

	// MemoryMap 内存文件映射
	MemoryMap
)

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
	// Size 获取到文件大小
	Size() (int64, error)
}

// 初始化 IOManager，目前只支持 FileIO
func NewIOManager(fileName string, ioType FileIOType) (IOManager, error) {
	switch ioType {
	case StandardFIO:
		return NewFileIOManager(fileName)
	case MemoryMap:
		return NewMMapIOManager(fileName)
	default:
		panic("unsupported io type")
	}

}
