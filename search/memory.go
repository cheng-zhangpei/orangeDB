package search

import (
	"ComDB"
	"ComDB/search/match_method"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/emirpasic/gods/queues/priorityqueue"
	"github.com/yanyiwu/gojieba"
	"math"
	"sort"
	"strings"
	"time"
)

// memoryMeta 记忆空间元数据
type memoryMeta struct {
	agentId    string                     // agentID
	memorySize int64                      // 当前记忆大小
	totalSize  int64                      // 总记忆大小
	match      *match_method.TFIDFMatcher // TF-IDF 匹配器
	timesHeap  *priorityqueue.Queue       // 时间戳堆
	jieba      *gojieba.Jieba             // Jieba 分词器（不持久化）
}

func NewMemoryMeta(agentId string, totalSize int64) *memoryMeta {
	// 创建一个最大堆，时间戳越大优先级越高
	pq := priorityqueue.NewWith(Int64Comparator) // 使用自定义比较器
	return &memoryMeta{
		agentId:    agentId,
		timesHeap:  pq,
		totalSize:  totalSize,
		memorySize: 0, // 初始 memorySize 为 0
		match:      match_method.NewTFIDFMatcher(),
		jieba:      gojieba.NewJieba(), // 初始化 Jieba 分词器
	}
}

// ----------------------|-----------------------|-----------------------|-----------------------|------------------------------|
//
// IdOffset |	agentId  	   |memorySize	  |	totalSize		|	HeapSize	    |	timeStampHeap							|
//
// ----------------------|-----------------------|-----------------------|-----------------------|-----------------------------|
func (mm *memoryMeta) encode() []byte {
	// 计算 agentId 的长度
	agentIdSize := len(mm.agentId)

	// 计算堆的大小
	heapSize := mm.timesHeap.Size()

	// 将 match 的 documents 和 df 序列化为 JSON
	matchJSON, err := json.Marshal(map[string]interface{}{
		"documents": mm.match.Documents,
		"df":        mm.match.Df,
		"totalDocs": mm.match.TotalDocs,
	})

	if err != nil {
		panic(fmt.Sprintf("failed to marshal match: %v", err))
	}
	matchSize := len(matchJSON)

	// 计算缓冲区大小
	// agentIdSize (8字节) + agentId (变长) + memorySize (变长) + totalSize (变长) + heapSize (变长) + 时间戳 (heapSize * 8字节) + matchSize (变长) + matchJSON (变长)
	bufSize := 8 + agentIdSize + binary.MaxVarintLen64*3 + heapSize*binary.MaxVarintLen64 + binary.MaxVarintLen64 + matchSize
	buf := make([]byte, bufSize)

	index := 0

	// 存储 agentId 的长度（8字节，小端存储）
	binary.LittleEndian.PutUint64(buf[index:index+8], uint64(agentIdSize))
	index += 8

	// 存储 agentId
	copy(buf[index:index+agentIdSize], mm.agentId)
	index += agentIdSize

	// 存储 memorySize（变长编码）
	index += binary.PutVarint(buf[index:], mm.memorySize)

	// 存储 totalSize（变长编码）
	index += binary.PutVarint(buf[index:], mm.totalSize)

	// 存储堆的大小（变长编码）
	index += binary.PutVarint(buf[index:], int64(heapSize))

	// 存储堆中的每个时间戳（变长编码）
	items := mm.timesHeap.Values()
	for _, item := range items {
		timestamp := item.(int64)
		index += binary.PutVarint(buf[index:], timestamp)
	}

	// 存储 match 的长度（变长编码）
	index += binary.PutVarint(buf[index:], int64(matchSize))

	// 存储 match 的 JSON 数据
	copy(buf[index:index+matchSize], matchJSON)
	index += matchSize

	// 返回实际写入的字节数据
	return buf[:index]
}
func decodeMemoryMeta(data []byte) (*memoryMeta, error) {
	// 这里
	mm := &memoryMeta{
		timesHeap: priorityqueue.NewWith(Int64Comparator), // 使用自定义的 Int64Comparator
		match:     match_method.NewTFIDFMatcher(),
		jieba:     gojieba.NewJieba(), // 初始化 Jieba 分词器
	}
	index := 0

	// 读取 agentId 的长度（8字节，小端存储）
	if len(data) < index+8 {
		return nil, fmt.Errorf("invalid data: insufficient bytes for agentId size")
	}
	agentIdSize := int(binary.LittleEndian.Uint64(data[index : index+8]))
	index += 8

	// 读取 agentId
	if len(data) < index+agentIdSize {
		return nil, fmt.Errorf("invalid data: insufficient bytes for agentId")
	}
	mm.agentId = string(data[index : index+agentIdSize])
	index += agentIdSize

	// 读取 memorySize（变长编码）
	memorySize, n := binary.Varint(data[index:])
	if n <= 0 {
		return nil, fmt.Errorf("invalid data: failed to decode memorySize")
	}
	mm.memorySize = memorySize
	index += n

	// 读取 totalSize（变长编码）
	totalSize, n := binary.Varint(data[index:])
	if n <= 0 {
		return nil, fmt.Errorf("invalid data: failed to decode totalSize")
	}
	mm.totalSize = totalSize
	index += n

	// 读取堆的大小（变长编码）
	heapSize, n := binary.Varint(data[index:])
	if n <= 0 {
		return nil, fmt.Errorf("invalid data: failed to decode heapSize")
	}
	index += n

	// 读取堆中的每个时间戳（变长编码）
	for i := 0; i < int(heapSize); i++ {
		timestamp, n := binary.Varint(data[index:])
		if n <= 0 {
			return nil, fmt.Errorf("invalid data: failed to decode timestamp at index %d", i)
		}
		mm.timesHeap.Enqueue(timestamp)
		index += n
	}

	// 读取 match 的长度（变长编码）
	matchSize, n := binary.Varint(data[index:])
	if n <= 0 {
		return nil, fmt.Errorf("invalid data: failed to decode match size")
	}
	index += n

	// 读取 match 的 JSON 数据
	if len(data) < index+int(matchSize) {
		return nil, fmt.Errorf("invalid data: insufficient bytes for match")
	}
	matchJSON := data[index : index+int(matchSize)]
	index += int(matchSize)

	// 反序列化 match
	var matchData struct {
		Documents []string       `json:"documents"`
		Df        map[string]int `json:"df"`
		TotalDocs int            `json:"totalDocs"`
	}
	if err := json.Unmarshal(matchJSON, &matchData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal match: %v", err)
	}
	mm.match.Documents = matchData.Documents
	mm.match.Df = matchData.Df
	mm.match.TotalDocs = matchData.TotalDocs

	return mm, nil
}

// 比较器
func Int64Comparator(a, b interface{}) int {
	aInt64 := a.(int64)
	bInt64 := b.(int64)
	switch {
	case aInt64 > bInt64:
		return 1
	case aInt64 < bInt64:
		return -1
	default:
		return 0
	}
}

// ============================= memory heap ====================================

// AddTimestamp 添加时间戳到堆中，并更新 memorySize
func (mm *memoryMeta) AddTimestamp(timestamp int64) {
	// 如果堆已满，移除最旧的时间戳（堆顶元素）
	if mm.timesHeap.Size() > 0 && mm.memorySize >= mm.totalSize {
		_, _ = mm.timesHeap.Dequeue()
		mm.memorySize -= 1
	}
	// 添加新的时间戳
	mm.timesHeap.Enqueue(timestamp)
	mm.memorySize += 1
}

// GetLatestTimestamp 获取最新的时间戳（堆顶元素）
func (mm *memoryMeta) GetLatestTimestamp() (int64, bool) {
	if mm.timesHeap.Size() == 0 {
		return 0, false
	}
	latest, _ := mm.timesHeap.Peek()
	return latest.(int64), true
}

// GetMemorySize 获取当前 memorySize
func (mm *memoryMeta) GetMemorySize() int64 {
	return mm.memorySize
}
func (mm *memoryMeta) GetAllMemory() []int64 {
	// 获取堆中的所有元素
	items := mm.timesHeap.Values()

	// 将元素转换为 int64 类型
	timestamps := make([]int64, len(items))
	for i, item := range items {
		timestamps[i] = item.(int64)
	}

	// 按时间戳从大到小排序（因为堆是最大堆）
	sort.Slice(timestamps, func(i, j int) bool {
		return timestamps[i] > timestamps[j]
	})
	return timestamps
}

// MemoryStructure ================================= memory search===============================
type MemoryStructure struct {
	Db *ComDB.DB
	mm *memoryMeta
}

// NewMemoryStructure 给智能体创建记忆空间
func NewMemoryStructure(opts ComDB.Options, agentId string, totalSize int64) (*MemoryStructure, error) {
	db, err := ComDB.Open(opts)
	if err != nil {
		return nil, err
	}
	memoryMetaData := NewMemoryMeta(agentId, totalSize)
	return &MemoryStructure{
		Db: db,
		mm: memoryMetaData,
	}, nil
}

// MMGet 获取记忆:此处为获取所有的记忆
func (ms *MemoryStructure) MMGet(agentId string) (string, error) {
	var meta *memoryMeta = nil
	meta, err := ms.findMetaData([]byte(agentId))
	if err != nil && !errors.Is(err, ComDB.ErrMemoryMetaNotFound) {
		return "", err
	}
	// 如果meta不存在的话
	if errors.Is(err, ComDB.ErrMemoryMetaNotFound) {
		// 此处给一个记忆空间的默认值，后续要提供修改记忆空间大小的值
		meta = NewMemoryMeta(agentId, 10)
	}
	// 获取所有的数据
	ms.mm = meta
	timeStamp := ms.mm.GetAllMemory()
	var memory string = ""
	for i, timeStamp := range timeStamp {
		// 构建真实的key
		realKey := getSearchKey(timeStamp, agentId)
		value, err := ms.Db.Get(realKey)
		// 堆中的数据必须都要存在
		if err != nil {
			return "", err
		}
		searchRecord, err := DecodeSearchRecord(value)
		memory += fmt.Sprintf("timeNear:%d,value:%s\n", i, string(searchRecord.dataField))
	}
	return memory, nil
}

// MMSet 设置记忆
func (ms *MemoryStructure) MMSet(value []byte, agentId string) error {
	// 查找 meta 数据
	meta, err := ms.findMetaData([]byte(agentId))
	if err != nil && !errors.Is(err, ComDB.ErrMemoryMetaNotFound) {
		return err // 返回错误
	}
	// 如果 meta 不存在，创建一个默认的 meta
	if errors.Is(err, ComDB.ErrMemoryMetaNotFound) {
		meta = NewMemoryMeta(agentId, 10) // 默认记忆空间大小为 10
	}
	// 这里需要更新信息
	ms.mm = meta
	// 获取当前时间戳
	timeStamp := time.Now().UnixNano()
	// 构建真实的 key
	realKey := getSearchKey(timeStamp, agentId)
	// 构建 SearchRecord
	matches := ms.mm.match.GenerateMatches(string(value), ms.mm.jieba)
	searchRecord := &SearchRecord{
		matchField: matches, // 使用传入的 key 作为 matchField
		dataField:  value,   // 使用传入的 value 作为 dataField
	}
	// 保存match信息=>也就是简单更新一些文档的参数
	ms.mm.match.Store(string(value), ms.mm.jieba)
	// 编码 SearchRecord
	encodedRecord := searchRecord.Encode()
	// 创建事件
	var opts = ComDB.DefaultWriteBatchOptions
	wb := ms.Db.NewWriteBatch(opts)

	// 将编码后的数据存储到数据库
	_ = wb.Put(realKey, encodedRecord)
	// 将时间戳添加到 meta 的时间戳堆中
	meta.AddTimestamp(timeStamp)
	enMeta := ms.mm.encode()
	_ = wb.Put([]byte(agentId), enMeta)
	// 提交事务
	if err := wb.Commit(); err != nil {
		return err
	}
	return nil
}

// 得到匹配程度高的
func (ms *MemoryStructure) MatchSearch(searchItem string, agentId string) (string, error) {
	var meta *memoryMeta = nil
	meta, err := ms.findMetaData([]byte(agentId))
	if err != nil && !errors.Is(err, ComDB.ErrMemoryMetaNotFound) {
		return "", err
	}
	// 如果meta不存在的话
	if errors.Is(err, ComDB.ErrMemoryMetaNotFound) {
		// 此处给一个记忆空间的默认值，后续要提供修改记忆空间大小的值
		meta = NewMemoryMeta(agentId, 10)
	}
	ms.mm = meta
	// 相似性数组
	similarities := make([]float64, ms.mm.GetMemorySize())
	timeStamps := ms.mm.GetAllMemory()
	values := make([]string, ms.mm.GetMemorySize())
	for i, timeStamp := range timeStamps {
		realKey := getSearchKey(timeStamp, agentId)
		// 获取数据
		searchRecord, err := ms.Db.Get(realKey)
		if err != nil {
			return "", err
		}
		record, err := DecodeSearchRecord(searchRecord)
		if err != nil {
			return "", err
		}
		// 先初始化一个jieba，别放到Store里头？
		searchItemMatches := ms.mm.match.GenerateMatches(searchItem, ms.mm.jieba)
		similarity := ms.mm.match.Match(record.matchField, searchItemMatches)
		values[i] = string(record.dataField)
		similarities[i] = similarity
	}
	// todo ：是否有更加合理的相似度计算方式？

	// 计算平均相似性和标准差
	avgSimilarity, stdDev := calculateStats(similarities)

	// 设定动态阈值
	threshold := avgSimilarity + stdDev

	// 过滤出高于阈值的记忆
	var results []string
	for i, similarity := range similarities {
		if similarity >= threshold {
			results = append(results, values[i])
		}
	}

	// 如果没有高于阈值的记忆，返回空
	if len(results) == 0 {
		return "", nil
	}

	// 返回匹配的记忆
	return strings.Join(results, ", "), nil
}

// 计算平均值和标准差
func calculateStats(similarities []float64) (float64, float64) {
	sum := 0.0
	for _, s := range similarities {
		sum += s
	}
	avg := sum / float64(len(similarities))

	variance := 0.0
	for _, s := range similarities {
		variance += math.Pow(s-avg, 2)
	}
	stdDev := math.Sqrt(variance / float64(len(similarities)))

	return avg, stdDev
}

// =========================================================================================

// 根据用户的输入找到元数据
func (ms *MemoryStructure) findMetaData(key []byte) (*memoryMeta, error) {
	metaData, err := ms.Db.Get(key)
	if err != nil && !errors.Is(err, ComDB.ErrKeyNotFound) {
		return nil, err
	}
	var meta *memoryMeta
	var exists = true
	if errors.Is(err, ComDB.ErrKeyNotFound) {
		exists = false
	} else {
		meta, err = decodeMemoryMeta(metaData)
		if err != nil {
			return nil, err
		}
	}
	if !exists {
		// 到外层创建MemoryMeta
		return nil, ComDB.ErrMemoryMetaNotFound
	}
	return meta, nil
}
