package main

import (
	"bufio"
	"container/heap"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
)

// ---------- Huffman coding ----------

type HuffmanTree struct {
	Nodes []*HuffmanNode
	Tree  *HuffmanNode
	Codes map[uint16]string
}

type HuffmanNode struct {
	Symbol    uint16
	Frequency int
	Left      *HuffmanNode
	Right     *HuffmanNode
	index     int // for heap
}

type PriorityQueue []*HuffmanNode

func (pq PriorityQueue) Len() int { return len(pq) }
func (pq PriorityQueue) Less(i, j int) bool {
	return pq[i].Frequency < pq[j].Frequency
}
func (pq PriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}
func (pq *PriorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*HuffmanNode)
	item.index = n
	*pq = append(*pq, item)
}
func (pq *PriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*pq = old[0 : n-1]
	return item
}

// Загрузка дерева Хаффмана из бинарного представления (как в оригинале)
func NewHuffmanTreeFromCodes(r io.Reader) (*HuffmanTree, error) {
	var numCodes int32
	if err := binary.Read(r, binary.BigEndian, &numCodes); err != nil {
		return nil, err
	}
	if numCodes < 0 {
		return nil, fmt.Errorf("invalid number of codes: %d", numCodes)
	}
	type rawCode struct {
		Symbol uint16
		Left   int16
		Right  int16
	}
	codes := make([]rawCode, numCodes)
	for i := range codes {
		if err := binary.Read(r, binary.BigEndian, &codes[i]); err != nil {
			return nil, err
		}
	}

	ht := &HuffmanTree{
		Nodes: make([]*HuffmanNode, 0),
		Codes: make(map[uint16]string),
	}
	var build func(idx int) *HuffmanNode
	build = func(idx int) *HuffmanNode {
		c := codes[idx]
		if c.Left < 0 && c.Right < 0 {
			node := &HuffmanNode{Symbol: c.Symbol}
			ht.Nodes = append(ht.Nodes, node)
			return node
		}
		node := &HuffmanNode{Symbol: 0xFFFF} // non-leaf
		ht.Nodes = append(ht.Nodes, node)
		if c.Left >= 0 {
			node.Left = build(int(c.Left))
		}
		if c.Right >= 0 {
			node.Right = build(int(c.Right))
		}
		return node
	}
	ht.Tree = build(0)

	// Назначаем коды листьям
	var assign func(node *HuffmanNode, prefix string)
	assign = func(node *HuffmanNode, prefix string) {
		if node.Left == nil && node.Right == nil {
			ht.Codes[node.Symbol] = prefix
		} else {
			if node.Left != nil {
				assign(node.Left, prefix+"0")
			}
			if node.Right != nil {
				assign(node.Right, prefix+"1")
			}
		}
	}
	assign(ht.Tree, "")
	return ht, nil
}

// Построение дерева Хаффмана с нуля по набору символов
func BuildHuffmanTreeFromSymbols(symbols []uint16) *HuffmanTree {
	freq := make(map[uint16]int)
	for _, s := range symbols {
		freq[s]++
	}
	pq := make(PriorityQueue, 0)
	heap.Init(&pq)
	for sym, f := range freq {
		node := &HuffmanNode{Symbol: sym, Frequency: f}
		heap.Push(&pq, node)
	}
	if pq.Len() == 0 {
		return &HuffmanTree{Tree: &HuffmanNode{Symbol: 0xFFFF}, Codes: make(map[uint16]string)}
	}
	for pq.Len() > 1 {
		left := heap.Pop(&pq).(*HuffmanNode)
		right := heap.Pop(&pq).(*HuffmanNode)
		branch := &HuffmanNode{Symbol: 0xFFFF, Frequency: left.Frequency + right.Frequency, Left: left, Right: right}
		heap.Push(&pq, branch)
	}
	ht := &HuffmanTree{
		Tree:  heap.Pop(&pq).(*HuffmanNode),
		Codes: make(map[uint16]string),
		Nodes: make([]*HuffmanNode, 0),
	}

	// Собираем все узлы в порядке pre-order (как в оригинале)
	var collect func(node *HuffmanNode)
	collect = func(node *HuffmanNode) {
		ht.Nodes = append(ht.Nodes, node)
		if node.Left != nil {
			collect(node.Left)
		}
		if node.Right != nil {
			collect(node.Right)
		}
	}
	if ht.Tree != nil {
		collect(ht.Tree)
	}

	// Назначаем коды листьям
	var assign func(node *HuffmanNode, prefix string)
	assign = func(node *HuffmanNode, prefix string) {
		if node.Left == nil && node.Right == nil {
			ht.Codes[node.Symbol] = prefix
		} else {
			if node.Left != nil {
				assign(node.Left, prefix+"0")
			}
			if node.Right != nil {
				assign(node.Right, prefix+"1")
			}
		}
	}
	assign(ht.Tree, "")

	return ht
}

func (ht *HuffmanTree) Encode(symbols []uint16) string {
	bits := ""
	for _, s := range symbols {
		code, ok := ht.Codes[s]
		if !ok {
			panic(fmt.Sprintf("no code for symbol %d", s))
		}
		bits += code
	}
	return bits
}

func (ht *HuffmanTree) Decode(bits string) []uint16 {
	var res []uint16
	node := ht.Tree
	for _, b := range bits {
		if b == '0' {
			node = node.Left
		} else {
			node = node.Right
		}
		if node.Left == nil && node.Right == nil {
			res = append(res, node.Symbol)
			node = ht.Tree
		}
	}
	return res
}

// ---------- BIN file structures ----------

type CoalescedFile struct {
	IsGlobal   bool
	CodeTable  *HuffmanTree
	NumFiles   int32
	Files      []*CoalescedFileEntry
}

type CoalescedFileEntry struct {
	Path        string
	NumSections int32
	Sections    []*Section
}

type Section struct {
	Name     string
	NumPairs int32
	Pairs    []*Pair
}

type Pair struct {
	Key    string
	Length uint16
	Bits   string
	Value  string
}

// ---------- Basic binary read/write ----------

func readS32BE(r io.Reader) (int32, error) {
	var v int32
	err := binary.Read(r, binary.BigEndian, &v)
	return v, err
}

func readU16BE(r io.Reader) (uint16, error) {
	var v uint16
	err := binary.Read(r, binary.BigEndian, &v)
	return v, err
}

func writeS32BE(w io.Writer, v int32) error {
	return binary.Write(w, binary.BigEndian, v)
}

func writeU16BE(w io.Writer, v uint16) error {
	return binary.Write(w, binary.BigEndian, v)
}

func readU16LE(r io.Reader) (uint16, error) {
	var buf [2]byte
	_, err := io.ReadFull(r, buf[:])
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(buf[:]), nil
}

// ---------- String and data handling (matching original format) ----------

// readString читает строку: int32 длина, если <0 — UTF-16 LE, иначе ASCII
func readString(r io.Reader) (string, error) {
	count, err := readS32BE(r)
	if err != nil {
		return "", err
	}
	if count < 0 {
		length := -count
		runes := make([]uint16, length)
		for i := int32(0); i < length; i++ {
			v, err := readU16LE(r)
			if err != nil {
				return "", err
			}
			runes[i] = v
		}
		if len(runes) > 0 && runes[len(runes)-1] == 0 {
			runes = runes[:len(runes)-1]
		}
		return string(utf16.Decode(runes)), nil
	} else {
		length := count
		bytes := make([]byte, length)
		_, err := io.ReadFull(r, bytes)
		if err != nil {
			return "", err
		}
		if len(bytes) > 0 && bytes[len(bytes)-1] == 0 {
			bytes = bytes[:len(bytes)-1]
		}
		return string(bytes), nil
	}
}

// writeString всегда записывает строку как UTF-16 LE (отрицательная длина), как в оригинале
func writeString(w io.Writer, s string) error {
	runes := utf16.Encode([]rune(s + "\x00"))
	count := -len(runes)
	if err := writeS32BE(w, int32(count)); err != nil {
		return err
	}
	for _, r := range runes {
		if err := binary.Write(w, binary.LittleEndian, r); err != nil {
			return err
		}
	}
	return nil
}

// readData читает сжатые данные: длина значения + битовый поток
func readData(r io.Reader) (uint16, string, error) {
	count, err := readS32BE(r)
	if err != nil {
		return 0, "", err
	}
	if count > 0 {
		return 0, "", fmt.Errorf("expected non-positive data length, got %d", count)
	}
	data := make([]byte, -count)
	_, err = io.ReadFull(r, data)
	if err != nil {
		return 0, "", err
	}
	if len(data) < 2 {
		return 0, "", fmt.Errorf("data too short for length prefix")
	}
	length := binary.BigEndian.Uint16(data[0:2])
	bitsData := data[2:]
	if length > 0 {
		// реверсируем байты
		reversed := make([]byte, len(bitsData))
		for i := 0; i < len(bitsData); i++ {
			reversed[len(bitsData)-1-i] = bitsData[i]
		}
		// переводим в биты
		bitsStr := ""
		for _, b := range reversed {
			for j := 7; j >= 0; j-- {
				if (b>>uint(j))&1 == 1 {
					bitsStr += "1"
				} else {
					bitsStr += "0"
				}
			}
		}
		// убираем ведущие нули
		bitsStr = strings.TrimLeft(bitsStr, "0")
		// реверсируем обратно (обратное к оригинальному bits[::-1])
		runes := []rune(bitsStr)
		for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
			runes[i], runes[j] = runes[j], runes[i]
		}
		bitsStr = string(runes)
		return length, bitsStr, nil
	}
	return length, "", nil
}

// writeData записывает сжатые данные в оригинальном формате
func writeData(w io.Writer, length uint16, bits string) error {
	var data []byte
	lenBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(lenBytes, length)
	data = append(data, lenBytes...)
	if length > 0 {
		// 1. Реверсируем битовую строку (как в питоне bits[::-1])
		revBits := reverseString(bits)
		// 2. Дополняем ведущими нулями до кратности 8
		padLen := (8 - len(revBits)%8) % 8
		padded := strings.Repeat("0", padLen) + revBits
		// 3. Преобразуем в байты (старший бит первый)
		bytesLen := len(padded) / 8
		rawBytes := make([]byte, bytesLen)
		for i := 0; i < bytesLen; i++ {
			b := byte(0)
			for j := 0; j < 8; j++ {
				if padded[i*8+j] == '1' {
					b |= 1 << (7 - j)
				}
			}
			rawBytes[i] = b
		}
		// 4. Реверсируем порядок байт (как [::-1] в питоне)
		for i, j := 0, len(rawBytes)-1; i < j; i, j = i+1, j-1 {
			rawBytes[i], rawBytes[j] = rawBytes[j], rawBytes[i]
		}
		data = append(data, rawBytes...)
	}
	// Записываем отрицательную длину блока данных
	if err := writeS32BE(w, -int32(len(data))); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func reverseString(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

// ---------- Coalesced file load/save ----------

func LoadCoalescedFile(r io.Reader, isGlobal bool) (*CoalescedFile, error) {
	cf := &CoalescedFile{IsGlobal: isGlobal}
	if isGlobal {
		ht, err := NewHuffmanTreeFromCodes(r)
		if err != nil {
			return nil, fmt.Errorf("failed to read code table: %v", err)
		}
		cf.CodeTable = ht
	}
	numFiles, err := readS32BE(r)
	if err != nil {
		return nil, err
	}
	cf.NumFiles = numFiles
	cf.Files = make([]*CoalescedFileEntry, numFiles)
	for i := int32(0); i < numFiles; i++ {
		entry := &CoalescedFileEntry{}
		entry.Path, err = readString(r)
		if err != nil {
			return nil, fmt.Errorf("failed to read file path: %v", err)
		}
		entry.NumSections, err = readS32BE(r)
		if err != nil {
			return nil, err
		}
		entry.Sections = make([]*Section, entry.NumSections)
		for j := int32(0); j < entry.NumSections; j++ {
			sec := &Section{}
			sec.Name, err = readString(r)
			if err != nil {
				return nil, err
			}
			sec.NumPairs, err = readS32BE(r)
			if err != nil {
				return nil, err
			}
			sec.Pairs = make([]*Pair, sec.NumPairs)
			for k := int32(0); k < sec.NumPairs; k++ {
				p := &Pair{}
				p.Key, err = readString(r)
				if err != nil {
					return nil, err
				}
				p.Length, p.Bits, err = readData(r)
				if err != nil {
					return nil, err
				}
				sec.Pairs[k] = p
			}
			entry.Sections[j] = sec
		}
		cf.Files[i] = entry
	}
	return cf, nil
}

func (cf *CoalescedFile) Save(w io.Writer) error {
	if cf.IsGlobal {
		if cf.CodeTable == nil {
			return fmt.Errorf("global file missing code table")
		}
		// Строим плоский список кодов в pre-order (как в оригинале)
		var codes []struct {
			symbol uint16
			left   int16
			right  int16
		}
		var build func(node *HuffmanNode)
		build = func(node *HuffmanNode) {
			code := struct {
				symbol uint16
				left   int16
				right  int16
			}{}
			if node.Left == nil && node.Right == nil {
				code.symbol = node.Symbol
				code.left = -1
				code.right = -1
			} else {
				code.symbol = 0xFFFF
				code.left = int16(indexOfNode(cf.CodeTable.Nodes, node.Left))
				code.right = int16(indexOfNode(cf.CodeTable.Nodes, node.Right))
			}
			codes = append(codes, code)
			if node.Left != nil {
				build(node.Left)
			}
			if node.Right != nil {
				build(node.Right)
			}
		}
		if cf.CodeTable.Tree != nil {
			build(cf.CodeTable.Tree)
		}
		if err := writeS32BE(w, int32(len(codes))); err != nil {
			return err
		}
		for _, c := range codes {
			if err := binary.Write(w, binary.BigEndian, c.symbol); err != nil {
				return err
			}
			if err := binary.Write(w, binary.BigEndian, c.left); err != nil {
				return err
			}
			if err := binary.Write(w, binary.BigEndian, c.right); err != nil {
				return err
			}
		}
	}
	if err := writeS32BE(w, cf.NumFiles); err != nil {
		return err
	}
	for _, file := range cf.Files {
		if err := writeString(w, file.Path); err != nil {
			return err
		}
		if err := writeS32BE(w, file.NumSections); err != nil {
			return err
		}
		for _, sec := range file.Sections {
			if err := writeString(w, sec.Name); err != nil {
				return err
			}
			if err := writeS32BE(w, sec.NumPairs); err != nil {
				return err
			}
			for _, p := range sec.Pairs {
				if err := writeString(w, p.Key); err != nil {
					return err
				}
				if err := writeData(w, p.Length, p.Bits); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func indexOfNode(nodes []*HuffmanNode, node *HuffmanNode) int {
	if node == nil {
		return -1
	}
	for i, n := range nodes {
		if n == node {
			return i
		}
	}
	return -1
}

// Декодируем значения всех пар, используя таблицу Хаффмана
func (cf *CoalescedFile) Decode(ht *HuffmanTree) {
	for _, file := range cf.Files {
		for _, sec := range file.Sections {
			for _, p := range sec.Pairs {
				if p.Length > 0 {
					symbols := ht.Decode(p.Bits)
					runes := make([]rune, len(symbols))
					for i, s := range symbols {
						runes[i] = rune(s)
					}
					p.Value = string(runes)
				} else {
					p.Value = ""
				}
			}
		}
	}
}

// Кодируем значения обратно в биты
func (cf *CoalescedFile) Encode(ht *HuffmanTree) {
	for _, file := range cf.Files {
		for _, sec := range file.Sections {
			for _, p := range sec.Pairs {
				if len(p.Value) > 0 {
					// Используем руны для правильного подсчёта количества символов
					runes := []rune(p.Value)
					symbols := make([]uint16, len(runes))
					for i, r := range runes {
						symbols[i] = uint16(r)
					}
					p.Bits = ht.Encode(symbols)
				} else {
					p.Bits = ""
				}
			}
		}
	}
}

// ---------- INI file handling ----------

type INIFile struct {
	Sections []*INISection
}

type INISection struct {
	Name  string
	Pairs []*INIPair
}

type INIPair struct {
	Key   string
	Value string
}

// LoadINI читает UTF-16 LE .ini файл с BOM
func LoadINI(r io.Reader) (*INIFile, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(data) < 2 {
		return &INIFile{}, nil
	}
	if data[0] != 0xFF || data[1] != 0xFE {
		return nil, fmt.Errorf("INI file does not have UTF-16 LE BOM")
	}
	u16 := make([]uint16, (len(data)-2)/2)
	for i := 0; i < len(u16); i++ {
		u16[i] = binary.LittleEndian.Uint16(data[2+i*2:])
	}
	content := string(utf16.Decode(u16))

	ini := &INIFile{}
	var currentSec *INISection
	sectionRegex := regexp.MustCompile(`^\[\s*(.*?)\s*\]$`)
	keyValueRegex := regexp.MustCompile(`^\s*(.*?)\s*=(.*)$`)
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimRight(line, "\r\n")
		if len(line) == 0 || strings.HasPrefix(line, ";") {
			continue
		}
		if m := sectionRegex.FindStringSubmatch(line); m != nil {
			sec := &INISection{Name: m[1]}
			ini.Sections = append(ini.Sections, sec)
			currentSec = sec
			continue
		}
		if currentSec == nil {
			continue
		}
		if m := keyValueRegex.FindStringSubmatch(line); m != nil {
			key := m[1]
			value := unescape(m[2])
			found := false
			for _, p := range currentSec.Pairs {
				if p.Key == key && p.Value == value {
					found = true
					break
				}
			}
			if !found {
				currentSec.Pairs = append(currentSec.Pairs, &INIPair{Key: key, Value: value})
			}
		}
	}
	return ini, scanner.Err()
}

// SaveINI сохраняет INIFile как UTF-16 LE с BOM (гарантированно 2 байта на символ)
func SaveINI(w io.Writer, ini *INIFile) error {
	var sb strings.Builder
	for _, sec := range ini.Sections {
		sb.WriteString(fmt.Sprintf("[%s]\r\n", sec.Name))
		for _, p := range sec.Pairs {
			sb.WriteString(fmt.Sprintf("%s=%s\r\n", p.Key, escape(p.Value)))
		}
		sb.WriteString("\r\n")
	}
	content := sb.String()
	runes := utf16.Encode([]rune(content))
	// Формируем буфер вручную: 2 байта BOM + по 2 байта на каждый код
	buf := make([]byte, 2+len(runes)*2)
	buf[0] = 0xFF
	buf[1] = 0xFE
	for i, r := range runes {
		buf[2+i*2] = byte(r)
		buf[2+i*2+1] = byte(r >> 8)
	}
	_, err := w.Write(buf)
	return err
}

func unescape(s string) string {
	// В режиме quotes=False (как при pack) мы не удаляем внешние кавычки и не делаем TrimSpace.
	// Только заменяем экранированные последовательности.
	s = strings.Replace(s, "\\\"", "\"", -1)
	// При необходимости можно добавить другие escape-последовательности
	return s
}

func escape(s string) string {
	// Экранируем спецсимволы, но не добавляем внешние кавычки (как в оригинале)
	s = strings.Replace(s, "\"", "\\\"", -1)
	s = strings.Replace(s, "\r", "\\r", -1)
	s = strings.Replace(s, "\n", "\\n", -1)
	return s
}

// ---------- Commands ----------

func commandUnpack(binDir, outDir string) error {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return err
	}
	var global *HuffmanTree
	coalescedFiles := make(map[string]*CoalescedFile)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToUpper(entry.Name()), ".BIN") {
			continue
		}
		fullPath := filepath.Join(binDir, entry.Name())
		f, err := os.Open(fullPath)
		if err != nil {
			return err
		}
		isGlobal := strings.HasPrefix(strings.ToUpper(entry.Name()), "COALESCED_")
		cf, err := LoadCoalescedFile(f, isGlobal)
		f.Close()
		if err != nil {
			return fmt.Errorf("error loading %s: %v", entry.Name(), err)
		}
		if cf.IsGlobal {
			if global != nil {
				return fmt.Errorf("multiple global coalesced files found")
			}
			global = cf.CodeTable
		}
		coalescedFiles[entry.Name()] = cf
	}
	if len(coalescedFiles) == 0 {
		return fmt.Errorf("no .bin files found")
	}
	if global == nil {
		return fmt.Errorf("no code table found (missing COALESCED_ file)")
	}
	for _, cf := range coalescedFiles {
		cf.Decode(global)
	}
	for _, cf := range coalescedFiles {
		for _, file := range cf.Files {
			ini := &INIFile{}
			for _, sec := range file.Sections {
				inis := &INISection{Name: sec.Name}
				for _, p := range sec.Pairs {
					runes := []rune(p.Value)
					if p.Length > 0 && len(runes) >= int(p.Length) {
						runes = runes[:p.Length-1]
					} else if len(runes) > 0 && runes[len(runes)-1] == 0 {
						runes = runes[:len(runes)-1]
					}
					inis.Pairs = append(inis.Pairs, &INIPair{Key: p.Key, Value: string(runes)})
				}
				ini.Sections = append(ini.Sections, inis)
			}
			relPath := strings.TrimPrefix(file.Path, "..\\..\\")
			relPath = strings.Replace(relPath, "\\", string(os.PathSeparator), -1)
			outPath := filepath.Join(outDir, relPath)
			if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
				return err
			}
			iniFile, err := os.Create(outPath)
			if err != nil {
				return err
			}
			err = SaveINI(iniFile, ini)
			iniFile.Close()
			if err != nil {
				return err
			}
		}
	}
	fmt.Println("Unpack completed successfully.")
	return nil
}

func commandList(binDir, listingPath string) error {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return err
	}
	outFile, err := os.Create(listingPath)
	if err != nil {
		return err
	}
	defer outFile.Close()
	writer := bufio.NewWriter(outFile)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToUpper(entry.Name()), ".BIN") {
			continue
		}
		fullPath := filepath.Join(binDir, entry.Name())
		f, err := os.Open(fullPath)
		if err != nil {
			return err
		}
		isGlobal := strings.HasPrefix(strings.ToUpper(entry.Name()), "COALESCED_")
		cf, err := LoadCoalescedFile(f, isGlobal)
		f.Close()
		if err != nil {
			return fmt.Errorf("error loading %s: %v", entry.Name(), err)
		}
		paths := make([]string, len(cf.Files))
		for i, file := range cf.Files {
			paths[i] = file.Path
		}
		line := entry.Name() + ":" + strings.Join(paths, ":") + "\n"
		if _, err := writer.WriteString(line); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	fmt.Printf("Listing written to %s\n", listingPath)
	return nil
}

func commandPack(listingPath, inputDir, outputDir string) error {
	f, err := os.Open(listingPath)
	if err != nil {
		return err
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	coalescedFiles := make(map[string]*CoalescedFile)
	allSymbols := make([]uint16, 0)
	for lines.Scan() {
		line := lines.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 2 {
			continue
		}
		binName := parts[0]
		if !strings.HasSuffix(strings.ToUpper(binName), ".BIN") {
			continue
		}
		iniPaths := strings.Split(parts[1], ":")
		isGlobal := strings.HasPrefix(strings.ToUpper(binName), "COALESCED_")
		cf := &CoalescedFile{IsGlobal: isGlobal}
		for _, iniRelPath := range iniPaths {
			if iniRelPath == "" {
				continue
			}
			iniRel := strings.TrimPrefix(iniRelPath, "..\\..\\")
			iniRel = strings.Replace(iniRel, "\\", string(os.PathSeparator), -1)
			iniFullPath := filepath.Join(inputDir, iniRel)
			iniF, err := os.Open(iniFullPath)
			if err != nil {
				return fmt.Errorf("cannot open ini file %s: %v", iniFullPath, err)
			}
			ini, err := LoadINI(iniF)
			iniF.Close()
			if err != nil {
				return err
			}
			entry := &CoalescedFileEntry{
				Path: iniRelPath,
			}
			for _, sec := range ini.Sections {
				cSec := &Section{Name: sec.Name}
				for _, p := range sec.Pairs {
					// Длина в символах, включая завершающий \0
					runes := []rune(p.Value)
					length := uint16(len(runes) + 1)
					value := p.Value + "\x00"
					cPair := &Pair{Key: p.Key, Length: length, Value: value}
					cSec.Pairs = append(cSec.Pairs, cPair)
					for _, r := range value {
						allSymbols = append(allSymbols, uint16(r))
					}
				}
				cSec.NumPairs = int32(len(cSec.Pairs))
				entry.Sections = append(entry.Sections, cSec)
			}
			entry.NumSections = int32(len(entry.Sections))
			cf.Files = append(cf.Files, entry)
		}
		cf.NumFiles = int32(len(cf.Files))
		coalescedFiles[binName] = cf
	}
	ht := BuildHuffmanTreeFromSymbols(allSymbols)
	for _, cf := range coalescedFiles {
		if cf.IsGlobal {
			cf.CodeTable = ht
		}
		cf.Encode(ht)
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return err
	}
	for binName, cf := range coalescedFiles {
		outPath := filepath.Join(outputDir, binName)
		outF, err := os.Create(outPath)
		if err != nil {
			return err
		}
		err = cf.Save(outF)
		outF.Close()
		if err != nil {
			return err
		}
	}
	fmt.Println("Pack completed successfully.")
	return nil
}

// ---------- Main ----------

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Coalesced tool for BioShock Infinite (PS3)")
		fmt.Printf("Usage: %s <command> [options]\n", filepath.Base(os.Args[0]))
		fmt.Println("Commands:")
		fmt.Println("  unpack <bin directory> <output directory>")
		fmt.Println("  pack   <listing file> <input directory> <output directory>")
		fmt.Println("  list   <bin directory> <listing file>")
		os.Exit(1)
	}
	cmd := os.Args[1]
	switch cmd {
	case "unpack":
		if len(os.Args) < 4 {
			fmt.Println("error: insufficient options for unpack")
			os.Exit(1)
		}
		binDir := os.Args[2]
		outDir := os.Args[3]
		if err := commandUnpack(binDir, outDir); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "list":
		if len(os.Args) < 4 {
			fmt.Println("error: insufficient options for list")
			os.Exit(1)
		}
		binDir := os.Args[2]
		listFile := os.Args[3]
		if err := commandList(binDir, listFile); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "pack":
		if len(os.Args) < 5 {
			fmt.Println("error: insufficient options for pack")
			os.Exit(1)
		}
		listFile := os.Args[2]
		inDir := os.Args[3]
		outDir := os.Args[4]
		if err := commandPack(listFile, inDir, outDir); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Printf("error: unknown command '%s'\n", cmd)
		os.Exit(1)
	}
}