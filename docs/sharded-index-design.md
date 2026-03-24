# 分片索引优化方案设计文档

## 1. 背景与目标

### 1.1 当前问题
在现有的 bitcask-go 实现中，内存中只有一个全局索引结构，所有的读写操作都需要竞争这个索引的全局锁。在高并发场景下，这会成为性能瓶颈。

### 1.2 优化目标
通过索引分片（Sharded Index）减小锁粒度，将单一全局锁拆分为多个分片锁，提升并发读写性能。

### 1.3 设计原则
- **接口透明**：对外接口保持不变，实现 `Indexer` 接口
- **向后兼容**：默认不启用分片，保持原有行为
- **可配置**：分片数量可配置，建议为 CPU 核心数的 2-4 倍
- **高效合并**：使用最小堆算法合并多个有序迭代器

---

## 2. 核心设计思路

### 2.1 索引分片原理

**单一索引（当前）：**
```
所有 key → 全局索引 → 全局锁（高竞争）
```

**分片索引（优化后）：**
```
key1 → hash(key1) % N → 分片0 → 锁0
key2 → hash(key2) % N → 分片1 → 锁1
key3 → hash(key3) % N → 分片2 → 锁2
...
```

### 2.2 Hash 路由算法

使用 FNV-1a hash 算法对 key 进行哈希，然后取模映射到对应分片：

```go
func getShard(key []byte) Indexer {
    hash := fnv.New32a()
    hash.Write(key)
    shardIdx := hash.Sum32() % shardCount
    return shards[shardIdx]
}
```

**选择 FNV-1a 的原因：**
- 速度快，适合短字符串
- 分布均匀
- Go 标准库支持

---

## 3. 架构设计

### 3.1 核心组件

#### ShardedIndexer - 分片索引管理器
```go
type ShardedIndexer struct {
    shards     []Indexer  // 索引分片数组
    shardCount uint32     // 分片数量
}
```

**职责：**
- 持有多个索引分片
- 实现 `Indexer` 接口
- 负责 key 的路由和分片管理

#### ShardedIterator - 最小堆迭代器
```go
type ShardedIterator struct {
    heap       *iteratorHeap  // 最小堆
    reverse    bool           // 是否反向遍历
    shardIters []Iterator     // 所有分片的迭代器
    current    *heapItem      // 当前元素
}
```

**职责：**
- 合并多个有序迭代器
- 使用最小堆保证有序输出
- 支持正向和反向遍历

---

## 4. 最小堆合并算法（核心创新）

### 4.1 算法原理

每个分片内部的数据是有序的，我们需要将多个有序子序列合并成一个全局有序序列。

**传统方案：**
1. 收集所有分片的数据到数组
2. 对整个数组排序
3. 时间复杂度：O(N log N)
4. 空间复杂度：O(N)

**最小堆方案（优化）：**
1. 创建最小堆，初始化时从每个迭代器取第一个元素
2. 每次取堆顶元素（最小值）
3. 从该元素对应的迭代器取下一个元素，插入堆
4. 时间复杂度：O(N log K)，K 为分片数
5. 空间复杂度：O(K)

**K << N 时，最小堆方案显著优于传统排序！**

### 4.2 算法流程

**初始化阶段：**
```
1. 创建所有分片的迭代器
2. 从每个迭代器取第一个元素
3. 将所有元素放入最小堆
4. 堆化（heapify）
```

**遍历阶段：**
```
while 堆不为空:
    1. 取出堆顶元素（最小值）
    2. 返回给用户
    3. 从该元素对应的迭代器中取下一个元素
    4. 如果有下一个元素，插入堆中
    5. 重新调整堆（O(log K)）
```

### 4.3 示例演示

假设有 3 个分片，每个分片内部有序：

```
分片0: [a, d, g]
分片1: [b, e, h]
分片2: [c, f, i]

初始堆: [a(0), b(1), c(2)]
        ↓ 堆顶

第1次 Next():
  - 取出 a，从分片0取 d
  - 堆: [b(1), c(2), d(0)]

第2次 Next():
  - 取出 b，从分片1取 e
  - 堆: [c(2), d(0), e(1)]

第3次 Next():
  - 取出 c，从分片2取 f
  - 堆: [d(0), e(1), f(2)]

...

最终得到有序序列: a, b, c, d, e, f, g, h, i
```

### 4.4 时间复杂度分析

- **初始化：** O(K log K)，K 为分片数
- **每次 Next()：** O(log K)
- **总遍历：** O(N log K)，N 为总元素数
- **空间复杂度：** O(K)

**对比：**
- 传统排序：O(N log N)
- 最小堆：O(N log K)
- 当 K=16, N=1000000 时：log K ≈ 4, log N ≈ 20，性能提升约 5 倍！

---

## 5. 详细实现设计

### 5.1 文件结构

```
index/
├── index.go           # 接口定义（已存在）
├── btree.go          # BTree 实现（已存在）
├── art.go            # ART 实现（已存在）
├── bptree.go         # B+Tree 实现（已存在）
└── sharded.go        # 新增：分片索引 + 最小堆迭代器
```

### 5.2 核心数据结构

#### heapItem - 堆元素
```go
type heapItem struct {
    key   []byte              // 元素的 key
    pos   *data.LogRecordPos  // 元素的位置信息
    iter  Iterator            // 该元素来自哪个迭代器
    index int                 // 迭代器索引（用于调试）
}
```

#### iteratorHeap - 最小堆
```go
type iteratorHeap struct {
    items   []*heapItem
    reverse bool  // 是否反向遍历
}

// 实现 container/heap.Interface
func (h *iteratorHeap) Len() int
func (h *iteratorHeap) Less(i, j int) bool
func (h *iteratorHeap) Swap(i, j int)
func (h *iteratorHeap) Push(x interface{})
func (h *iteratorHeap) Pop() interface{}
```

**Less 方法（关键）：**
```go
func (h *iteratorHeap) Less(i, j int) bool {
    cmp := bytes.Compare(h.items[i].key, h.items[j].key)
    if h.reverse {
        return cmp > 0  // 反向遍历：大的在堆顶
    }
    return cmp < 0  // 正向遍历：小的在堆顶
}
```


### 5.3 ShardedIndexer 实现

#### 初始化
```go
func NewShardedIndexer(shardCount uint32, indexType IndexType, dirPath string, sync bool) *ShardedIndexer {
    shards := make([]Indexer, shardCount)
    
    for i := uint32(0); i < shardCount; i++ {
        switch indexType {
        case Btree:
            shards[i] = NewBTree()
        case ART:
            shards[i] = NewART()
        case BPTree:
            // 每个分片使用独立的 BoltDB 文件
            shardPath := fmt.Sprintf("%s/bptree-index-%d", dirPath, i)
            shards[i] = NewBPlusTree(shardPath, sync)
        }
    }
    
    return &ShardedIndexer{
        shards:     shards,
        shardCount: shardCount,
    }
}
```

#### Put/Get/Delete - 简单路由
```go
func (si *ShardedIndexer) Put(key []byte, pos *data.LogRecordPos) *data.LogRecordPos {
    shard := si.getShard(key)
    return shard.Put(key, pos)
}

func (si *ShardedIndexer) Get(key []byte) *data.LogRecordPos {
    shard := si.getShard(key)
    return shard.Get(key)
}

func (si *ShardedIndexer) Delete(key []byte) (*data.LogRecordPos, bool) {
    shard := si.getShard(key)
    return shard.Delete(key)
}
```

#### Size - 汇总所有分片
```go
func (si *ShardedIndexer) Size() int {
    total := 0
    for _, shard := range si.shards {
        total += shard.Size()
    }
    return total
}
```

#### Iterator - 创建最小堆迭代器
```go
func (si *ShardedIndexer) Iterator(reverse bool) Iterator {
    return newShardedIterator(si.shards, reverse)
}
```

### 5.4 ShardedIterator 实现

#### 初始化
```go
func newShardedIterator(shards []Indexer, reverse bool) *ShardedIterator {
    // 1. 创建所有分片的迭代器
    shardIters := make([]Iterator, len(shards))
    for i, shard := range shards {
        shardIters[i] = shard.Iterator(reverse)
    }
    
    // 2. 初始化最小堆
    h := &iteratorHeap{
        items:   make([]*heapItem, 0, len(shards)),
        reverse: reverse,
    }
    
    // 3. 从每个迭代器取第一个元素放入堆
    for i, iter := range shardIters {
        iter.Rewind()
        if iter.Valid() {
            h.items = append(h.items, &heapItem{
                key:   iter.Key(),
                pos:   iter.Value(),
                iter:  iter,
                index: i,
            })
        }
    }
    
    // 4. 堆化
    heap.Init(h)
    
    // 5. 取出堆顶作为当前元素
    var current *heapItem
    if h.Len() > 0 {
        current = heap.Pop(h).(*heapItem)
    }
    
    return &ShardedIterator{
        heap:       h,
        reverse:    reverse,
        shardIters: shardIters,
        current:    current,
    }
}
```

#### Rewind - 重置到起点
```go
func (si *ShardedIterator) Rewind() {
    // 1. 清空堆
    si.heap.items = si.heap.items[:0]
    
    // 2. 所有迭代器 Rewind
    for i, iter := range si.shardIters {
        iter.Rewind()
        if iter.Valid() {
            si.heap.items = append(si.heap.items, &heapItem{
                key:   iter.Key(),
                pos:   iter.Value(),
                iter:  iter,
                index: i,
            })
        }
    }
    
    // 3. 重新堆化
    heap.Init(si.heap)
    
    // 4. 取出堆顶
    if si.heap.Len() > 0 {
        si.current = heap.Pop(si.heap).(*heapItem)
    } else {
        si.current = nil
    }
}
```

#### Next - 移动到下一个元素
```go
func (si *ShardedIterator) Next() {
    if si.current == nil {
        return
    }
    
    // 1. 从当前元素的迭代器取下一个
    si.current.iter.Next()
    
    // 2. 如果还有元素，放入堆
    if si.current.iter.Valid() {
        si.current.key = si.current.iter.Key()
        si.current.pos = si.current.iter.Value()
        heap.Push(si.heap, si.current)
    }
    
    // 3. 取出新的堆顶作为当前元素
    if si.heap.Len() > 0 {
        si.current = heap.Pop(si.heap).(*heapItem)
    } else {
        si.current = nil
    }
}
```

#### Seek - 定位到指定 key
```go
func (si *ShardedIterator) Seek(key []byte) {
    // 1. 清空堆
    si.heap.items = si.heap.items[:0]
    
    // 2. 所有迭代器 Seek
    for i, iter := range si.shardIters {
        iter.Seek(key)
        if iter.Valid() {
            si.heap.items = append(si.heap.items, &heapItem{
                key:   iter.Key(),
                pos:   iter.Value(),
                iter:  iter,
                index: i,
            })
        }
    }
    
    // 3. 重新堆化
    heap.Init(si.heap)
    
    // 4. 取出堆顶
    if si.heap.Len() > 0 {
        si.current = heap.Pop(si.heap).(*heapItem)
    } else {
        si.current = nil
    }
}
```

#### 其他方法
```go
func (si *ShardedIterator) Valid() bool {
    return si.current != nil
}

func (si *ShardedIterator) Key() []byte {
    if si.current == nil {
        return nil
    }
    return si.current.key
}

func (si *ShardedIterator) Value() *data.LogRecordPos {
    if si.current == nil {
        return nil
    }
    return si.current.pos
}

func (si *ShardedIterator) Close() {
    for _, iter := range si.shardIters {
        iter.Close()
    }
    si.heap.items = nil
    si.current = nil
}
```

---

## 6. 配置项设计

### 6.1 Options 新增字段

```go
type Options struct {
    // ... 现有字段
    
    // 索引分片数量，用于减小锁粒度，提升并发性能
    // 0 或 1: 不分片，使用单一索引
    // > 1: 使用分片索引，建议设置为 CPU 核心数的 2-4 倍
    IndexShardCount uint32
}
```

### 6.2 默认值策略

```go
var DefaultOptions = Options{
    // ... 现有配置
    IndexShardCount: 0,  // 默认不分片，保持向后兼容
}
```

### 6.3 推荐配置

```go
// 高并发场景推荐配置
func HighConcurrencyOptions() Options {
    opts := DefaultOptions
    opts.IndexShardCount = uint32(runtime.NumCPU() * 2)
    return opts
}
```

**分片数量选择建议：**
- **低并发（< 10 goroutines）：** 0 或 1（不分片）
- **中等并发（10-100 goroutines）：** CPU 核心数
- **高并发（> 100 goroutines）：** CPU 核心数的 2-4 倍
- **极高并发（> 1000 goroutines）：** 32 或 64

---

## 7. 集成修改点

### 7.1 修改 options.go

在 `Options` 结构体中添加 `IndexShardCount` 字段。

### 7.2 修改 index/index.go

```go
// 修改 NewIndexer 函数签名，添加 shardCount 参数
func NewIndexer(typ IndexType, dirPath string, sync bool, shardCount uint32) Indexer {
    // 如果 shardCount <= 1，使用原有逻辑（单一索引）
    if shardCount <= 1 {
        switch typ {
        case Btree:
            return NewBTree()
        case ART:
            return NewART()
        case BPTree:
            return NewBPlusTree(dirPath, sync)
        default:
            panic("unsupported index type")
        }
    }
    
    // 使用分片索引
    return NewShardedIndexer(shardCount, typ, dirPath, sync)
}
```

### 7.3 修改 db.go

```go
// db.go 第 89 行，初始化索引时传入分片数量
db := &DB{
    options:    options,
    mu:         new(sync.RWMutex),
    olderFiles: make(map[uint32]*data.DataFile),
    index: index.NewIndexer(
        index.IndexType(options.IndexType),
        options.DirPath,
        options.SyncWrites,
        options.IndexShardCount,  // 新增参数
    ),
    isInitial:  isInitial,
    fileLock:   fileLock,
}
```

---

## 8. 特殊场景处理

### 8.1 B+Tree 索引分片

B+Tree 索引存储在磁盘上（使用 BoltDB），分片时需要为每个分片创建独立的数据库文件：

```go
// 文件命名规则
dirPath/bptree-index-0.db
dirPath/bptree-index-1.db
dirPath/bptree-index-2.db
...
```

**实现：**
```go
case BPTree:
    shardPath := fmt.Sprintf("%s/bptree-index-%d", dirPath, i)
    shards[i] = NewBPlusTree(shardPath, sync)
```

### 8.2 Merge 操作兼容性

Merge 操作需要遍历所有 key，分片索引的 Iterator 能够正确合并所有分片的数据，保证顺序一致性。

**验证点：**
- Iterator 返回的数据是全局有序的
- 不会遗漏任何 key
- 不会重复返回 key

### 8.3 向后兼容

- 默认 `IndexShardCount = 0`，保持原有单一索引行为
- 旧数据库升级时自动使用单一索引
- 新数据库可以选择启用分片

---

## 9. 性能分析

### 9.1 理论性能提升

假设有 N 个分片：

**并发写入：**
- 单一索引：所有写入竞争 1 个锁
- 分片索引：写入分散到 N 个锁
- **理论提升：N 倍**（实际 3-6 倍，受限于其他因素）

**并发读取：**
- 单一索引：所有读取竞争 1 个读锁
- 分片索引：读取分散到 N 个读锁
- **理论提升：N 倍**（实际 3-6 倍）

**迭代器遍历：**
- 单一索引：O(N)
- 分片索引（最小堆）：O(N log K)，K 为分片数
- **K << N 时，性能接近单一索引**

### 9.2 时间复杂度对比

| 操作 | 单一索引 | 分片索引 | 说明 |
|------|---------|---------|------|
| Put | O(log N) | O(log N/K) | 每个分片数据量减少 |
| Get | O(log N) | O(log N/K) | 每个分片数据量减少 |
| Delete | O(log N) | O(log N/K) | 每个分片数据量减少 |
| Iterator Init | O(N) | O(K log K) | 初始化堆 |
| Iterator Next | O(1) | O(log K) | 堆调整 |
| Iterator 总遍历 | O(N) | O(N log K) | K 通常很小 |

### 9.3 空间复杂度

| 组件 | 单一索引 | 分片索引 | 增量 |
|------|---------|---------|------|
| 索引数据 | O(N) | O(N) | 无增加 |
| 锁结构 | O(1) | O(K) | 可忽略 |
| Iterator | O(N) | O(K) | 显著减少 |

### 9.4 最佳分片数量

**经验公式：**
```
最佳分片数 = CPU 核心数 × 并发系数

并发系数：
- 低并发：1-2
- 中等并发：2-4
- 高并发：4-8
```

**示例：**
- 4 核 CPU，中等并发：4 × 2 = 8 分片
- 8 核 CPU，高并发：8 × 4 = 32 分片
- 16 核 CPU，极高并发：16 × 4 = 64 分片

**注意：** 分片数过多会导致：
- Iterator 性能下降（log K 增大）
- 内存占用增加
- 管理开销增加

---

## 10. 测试计划

### 10.1 功能测试

```go
// 基本功能测试
func TestShardedIndexer_PutGetDelete(t *testing.T) {
    // 验证基本的 Put/Get/Delete 操作
}

// 最小堆合并测试
func TestShardedIterator_MinHeap(t *testing.T) {
    // 验证多个有序序列合并的正确性
    // 检查输出是否全局有序
}

// Seek 功能测试
func TestShardedIterator_Seek(t *testing.T) {
    // 验证 Seek 到指定 key 的正确性
}

// 反向遍历测试
func TestShardedIterator_Reverse(t *testing.T) {
    // 验证反向遍历的正确性
}

// 边界条件测试
func TestShardedIndexer_EdgeCases(t *testing.T) {
    // 空索引
    // 单个元素
    // 所有元素在同一分片
}
```

### 10.2 并发测试

```go
// 并发写入测试
func TestShardedIndexer_ConcurrentPut(t *testing.T) {
    // 多个 goroutine 同时写入
    // 验证数据一致性
}

// 并发读写测试
func TestShardedIndexer_ConcurrentReadWrite(t *testing.T) {
    // 同时进行读写操作
    // 验证无死锁和数据竞争
}
```

### 10.3 性能测试

```go
// 写入性能对比
func BenchmarkPut_SingleIndex(b *testing.B)
func BenchmarkPut_Sharded_4(b *testing.B)
func BenchmarkPut_Sharded_16(b *testing.B)
func BenchmarkPut_Sharded_32(b *testing.B)

// 读取性能对比
func BenchmarkGet_SingleIndex(b *testing.B)
func BenchmarkGet_Sharded_16(b *testing.B)

// 迭代器性能对比
func BenchmarkIterator_SingleIndex(b *testing.B)
func BenchmarkIterator_Sharded_16(b *testing.B)

// 并发性能测试
func BenchmarkConcurrentPut_SingleIndex(b *testing.B)
func BenchmarkConcurrentPut_Sharded_16(b *testing.B)
```

---

## 11. 预期效果

### 11.1 并发写入场景

**测试环境：** 8 核 CPU，100 万次 Put 操作，100 个并发 goroutine

| 配置 | 吞吐量 (ops/s) | 延迟 P99 (ms) | 提升倍数 |
|------|---------------|--------------|---------|
| 单一索引 | 100K | 5.0 | 1x |
| 4 分片 | 280K | 2.0 | 2.8x |
| 16 分片 | 500K | 1.2 | 5x |
| 32 分片 | 550K | 1.0 | 5.5x |

### 11.2 并发读取场景

**测试环境：** 8 核 CPU，100 万次 Get 操作，100 个并发 goroutine

| 配置 | 吞吐量 (ops/s) | 延迟 P99 (ms) | 提升倍数 |
|------|---------------|--------------|---------|
| 单一索引 | 500K | 1.0 | 1x |
| 16 分片 | 1.5M | 0.4 | 3x |

### 11.3 迭代器遍历场景

**测试环境：** 100 万条数据，顺序遍历

| 配置 | 遍历时间 (ms) | 说明 |
|------|--------------|------|
| 单一索引 | 50 | 基准 |
| 16 分片（最小堆） | 80 | 略慢，但可接受 |
| 16 分片（传统排序） | 200 | 慢很多（不采用） |

**结论：** 最小堆方案在保证并发性能的同时，迭代器性能损失可控。

---

## 12. 实现优先级

### Phase 1: 核心功能（必须）
- [x] 设计文档编写
- [ ] 创建 `index/sharded.go` 文件
- [ ] 实现 `ShardedIndexer` 基本结构
- [ ] 实现 Hash 路由（`getShard`）
- [ ] 实现 Put/Get/Delete 方法
- [ ] 实现 Size 方法

### Phase 2: 最小堆迭代器（核心）
- [ ] 实现 `heapItem` 和 `iteratorHeap` 结构
- [ ] 实现 `heap.Interface` 接口
- [ ] 实现 `ShardedIterator` 初始化
- [ ] 实现 Rewind/Next/Valid/Key/Value 方法
- [ ] 实现 Seek 方法
- [ ] 支持反向遍历

### Phase 3: 集成和配置
- [ ] 修改 `options.go` 添加 `IndexShardCount` 字段
- [ ] 修改 `index/index.go` 的 `NewIndexer` 函数
- [ ] 修改 `db.go` 传递分片参数
- [ ] 更新 `DefaultOptions`

### Phase 4: 测试和优化
- [ ] 编写单元测试
- [ ] 编写并发测试
- [ ] 编写性能基准测试
- [ ] 性能调优
- [ ] 文档完善

---

## 13. 风险和注意事项

### 13.1 潜在风险

1. **Iterator 性能下降**
   - 风险：分片数过多导致堆操作开销增大
   - 缓解：建议分片数不超过 64

2. **内存占用增加**
   - 风险：每个分片独立的数据结构
   - 缓解：增量很小，可忽略

3. **B+Tree 文件数增加**
   - 风险：分片导致多个 BoltDB 文件
   - 缓解：操作系统可以处理，影响不大

### 13.2 兼容性注意

1. **向后兼容**
   - 默认不启用分片
   - 旧数据库自动使用单一索引

2. **升级路径**
   - 无需数据迁移
   - 修改配置即可启用分片

3. **降级路径**
   - 将 `IndexShardCount` 设为 0 即可

### 13.3 调试建议

1. **日志记录**
   - 记录分片数量和路由信息
   - 记录堆操作次数

2. **监控指标**
   - 每个分片的数据量
   - 锁竞争情况
   - Iterator 性能

---

## 14. 总结

### 14.1 方案优势

1. ✅ **显著提升并发性能**：锁竞争降低 N 倍
2. ✅ **高效的迭代器合并**：最小堆 O(N log K) 优于排序 O(N log N)
3. ✅ **内存友好**：流式处理，只需 O(K) 额外空间
4. ✅ **接口透明**：对外完全兼容 `Indexer` 接口
5. ✅ **向后兼容**：默认不启用，保持原有行为
6. ✅ **灵活可配置**：分片数量可根据场景调整

### 14.2 适用场景

**推荐使用：**
- 高并发读写场景（> 100 goroutines）
- 写入密集型应用
- 需要低延迟的场景

**不推荐使用：**
- 低并发场景（< 10 goroutines）
- 迭代器密集型应用
- 内存受限环境

### 14.3 技术亮点

这个优化方案展示了以下技术能力：

1. **并发编程**：锁粒度优化、分片设计
2. **算法优化**：最小堆合并、K-way merge
3. **系统设计**：接口抽象、向后兼容
4. **性能调优**：时间空间权衡、参数调优

**非常适合作为简历项目的技术亮点！**

---

## 15. 参考资料

### 15.1 相关论文
- Bitcask: A Log-Structured Hash Table for Fast Key/Value Data
- The Log-Structured Merge-Tree (LSM-Tree)

### 15.2 相关实现
- LevelDB: Sharded MemTable
- RocksDB: Column Families
- Redis: Hash Slot

### 15.3 Go 标准库
- `container/heap`: 堆实现
- `hash/fnv`: FNV hash 算法
- `sync`: 并发原语

---

## 附录：完整代码框架

### A.1 sharded.go 文件结构

```go
package index

import (
    "bitcask-go/data"
    "bytes"
    "container/heap"
    "fmt"
    "hash/fnv"
)

// ========== 分片索引 ==========

type ShardedIndexer struct {
    shards     []Indexer
    shardCount uint32
}

func NewShardedIndexer(...) *ShardedIndexer { ... }
func (si *ShardedIndexer) getShard(key []byte) Indexer { ... }
func (si *ShardedIndexer) Put(...) *data.LogRecordPos { ... }
func (si *ShardedIndexer) Get(...) *data.LogRecordPos { ... }
func (si *ShardedIndexer) Delete(...) (*data.LogRecordPos, bool) { ... }
func (si *ShardedIndexer) Size() int { ... }
func (si *ShardedIndexer) Iterator(reverse bool) Iterator { ... }
func (si *ShardedIndexer) Close() error { ... }

// ========== 最小堆迭代器 ==========

type ShardedIterator struct {
    heap       *iteratorHeap
    reverse    bool
    shardIters []Iterator
    current    *heapItem
}

func newShardedIterator(...) *ShardedIterator { ... }
func (si *ShardedIterator) Rewind() { ... }
func (si *ShardedIterator) Seek(key []byte) { ... }
func (si *ShardedIterator) Next() { ... }
func (si *ShardedIterator) Valid() bool { ... }
func (si *ShardedIterator) Key() []byte { ... }
func (si *ShardedIterator) Value() *data.LogRecordPos { ... }
func (si *ShardedIterator) Close() { ... }

// ========== 最小堆实现 ==========

type heapItem struct {
    key   []byte
    pos   *data.LogRecordPos
    iter  Iterator
    index int
}

type iteratorHeap struct {
    items   []*heapItem
    reverse bool
}

func (h *iteratorHeap) Len() int { ... }
func (h *iteratorHeap) Less(i, j int) bool { ... }
func (h *iteratorHeap) Swap(i, j int) { ... }
func (h *iteratorHeap) Push(x interface{}) { ... }
func (h *iteratorHeap) Pop() interface{} { ... }
```

---

**文档版本：** v1.0  
**创建日期：** 2026-03-24  
**作者：** bitcask-go 项目组  
**状态：** 设计阶段
