package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Константы формата God of War 2
const (
	SectorSize   = 2048
	TocLbaOffset = 0x8068
	EntrySize    = 36 // 24 (имя) + 4 (размер) + 4 (archiveID) + 4 (offsetID)
	NameLen      = 24
	HeaderSize   = 4
)

// Entry представляет запись TOC
type Entry struct {
	Name      string `json:"name"`      // оригинальное имя (ASCII)
	Size      uint32 `json:"size"`      // размер в байтах
	ArchiveID uint32 `json:"archiveId"` // всегда 1 в новом TOC
	OffsetID  uint32 `json:"offsetId"`  // индекс в таблице LBA (после перепаковки)
	Lba       uint32 `json:"lba"`       // оригинальный LBA (только для информации)
	Pak       string `json:"pak"`       // исходный PAK (PART1/PART2)
	Offset    int64  `json:"offset"`    // исходное смещение
}

// Metadata – полный список записей для сохранения в JSON
type Metadata struct {
	FileCount uint32  `json:"fileCount"`
	Entries   []Entry `json:"entries"`
}

// ---------- Вспомогательные функции ----------

// safeNameFromBytes создаёт безопасное для Windows имя из сырых байт
func safeNameFromBytes(data []byte) string {
	var res strings.Builder
	for _, b := range data {
		if b == 0 {
			break
		}
		if b >= 32 && b <= 126 && !strings.ContainsRune(`\/:*?"<>|`, rune(b)) {
			res.WriteByte(b)
		} else {
			res.WriteByte('_')
		}
	}
	if res.Len() == 0 {
		return "file"
	}
	return res.String()
}

// ---------- Чтение TOC (распаковка) ----------

func readTOC(tocPath, pak1Path, pak2Path string) (*Metadata, error) {
	data, err := os.ReadFile(tocPath)
	if err != nil {
		return nil, fmt.Errorf("чтение TOC: %v", err)
	}
	if len(data) < HeaderSize {
		return nil, fmt.Errorf("TOC слишком мал")
	}
	fileCount := binary.LittleEndian.Uint32(data[:4])
	meta := &Metadata{FileCount: fileCount, Entries: make([]Entry, fileCount)}

	info1, _ := os.Stat(pak1Path)
	info2, _ := os.Stat(pak2Path)
	pak1Size := int64(0)
	pak2Size := int64(0)
	if info1 != nil {
		pak1Size = info1.Size()
	}
	if info2 != nil {
		pak2Size = info2.Size()
	}

	for i := uint32(0); i < fileCount; i++ {
		base := HeaderSize + int(i)*EntrySize
		if base+EntrySize > len(data) {
			return nil, fmt.Errorf("запись %d выходит за границы", i)
		}
		nameBytes := data[base : base+NameLen]
		end := 0
		for end < len(nameBytes) && nameBytes[end] != 0 {
			end++
		}
		name := string(nameBytes[:end])
		size := binary.LittleEndian.Uint32(data[base+NameLen : base+NameLen+4])
		archiveID := binary.LittleEndian.Uint32(data[base+NameLen+4 : base+NameLen+8])
		offsetID := binary.LittleEndian.Uint32(data[base+NameLen+8 : base+NameLen+12])
		meta.Entries[i] = Entry{
			Name:      name,
			Size:      size,
			ArchiveID: archiveID,
			OffsetID:  offsetID,
		}
	}

	// Таблица LBA
	lbaStart := TocLbaOffset
	if len(data) < lbaStart+int(fileCount)*4 {
		return nil, fmt.Errorf("таблица LBA за пределами")
	}
	for i := uint32(0); i < fileCount; i++ {
		off := lbaStart + int(i)*4
		lba := binary.LittleEndian.Uint32(data[off : off+4])
		meta.Entries[i].Lba = lba
		realOffset := int64(lba) * SectorSize
		if realOffset < pak1Size {
			meta.Entries[i].Pak = pak1Path
			meta.Entries[i].Offset = realOffset
		} else if realOffset < pak1Size+pak2Size {
			meta.Entries[i].Pak = pak2Path
			meta.Entries[i].Offset = realOffset - pak1Size
		} else {
			return nil, fmt.Errorf("смещение %d вне обоих PAK", realOffset)
		}
	}
	return meta, nil
}

// extractFiles – распаковка всех файлов с безопасными именами
func extractFiles(meta *Metadata, outDir string) error {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}
	handles := make(map[string]*os.File)
	defer func() {
		for _, f := range handles {
			f.Close()
		}
	}()

	for _, entry := range meta.Entries {
		if _, ok := handles[entry.Pak]; !ok {
			f, err := os.Open(entry.Pak)
			if err != nil {
				return fmt.Errorf("открытие %s: %v", entry.Pak, err)
			}
			handles[entry.Pak] = f
		}
		src := handles[entry.Pak]
		safeName := safeNameFromBytes([]byte(entry.Name))
		outPath := filepath.Join(outDir, safeName)
		if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
			return err
		}
		dst, err := os.Create(outPath)
		if err != nil {
			return err
		}
		_, err = src.Seek(entry.Offset, io.SeekStart)
		if err != nil {
			dst.Close()
			return err
		}
		_, err = io.CopyN(dst, src, int64(entry.Size))
		dst.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// saveMetadata – сохраняет метаданные в JSON
func saveMetadata(meta *Metadata, outDir string) error {
	path := filepath.Join(outDir, "_metadata.json")
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// loadMetadata – загружает метаданные из JSON
func loadMetadata(inDir string) (*Metadata, error) {
	path := filepath.Join(inDir, "_metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// computeHashFile – вычисляет CRC64 для файла (дедупликация)
func computeHashFile(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := crc64.New(crc64.MakeTable(crc64.ECMA))
	if _, err := io.Copy(h, f); err != nil {
		return 0, err
	}
	return h.Sum64(), nil
}

// ---------- Релинк по имени (без расширения) ----------

func relinkByName(entries []Entry) error {
	priorityExts := []string{".pss", ".psw"}
	groups := make(map[string][]int)
	for i, entry := range entries {
		base := strings.ToLower(filepath.Base(entry.Name))
		ext := filepath.Ext(base)
		nameWithoutExt := strings.TrimSuffix(base, ext)
		groups[nameWithoutExt] = append(groups[nameWithoutExt], i)
	}

	for _, indices := range groups {
		if len(indices) <= 1 {
			continue
		}
		// Выбираем оригинал с наивысшим приоритетом расширения
		var bestIdx int
		bestPriority := -1
		for _, idx := range indices {
			ext := strings.ToLower(filepath.Ext(entries[idx].Name))
			priority := -1
			for i, p := range priorityExts {
				if ext == p {
					priority = i
					break
				}
			}
			if priority > bestPriority {
				bestPriority = priority
				bestIdx = idx
			}
		}
		if bestPriority == -1 {
			bestIdx = indices[0]
		}
		// Перенаправляем все остальные на оригинал
		for _, idx := range indices {
			if idx == bestIdx {
				continue
			}
			entries[idx].OffsetID = entries[bestIdx].OffsetID
			entries[idx].ArchiveID = 1
		}
	}
	return nil
}

// ---------- Упаковка (pack) ----------

func pack(inDir, outPakPath, outTocPath string, dedup bool, relinkName bool) error {
	meta, err := loadMetadata(inDir)
	if err != nil {
		return fmt.Errorf("загрузка метаданных: %v", err)
	}

	var activeEntries []Entry

	// 1. Проходим по записям из метаданных, проверяем наличие файлов на диске
	for _, entry := range meta.Entries {
		safeName := safeNameFromBytes([]byte(entry.Name))
		filePath := filepath.Join(inDir, safeName)
		info, err := os.Stat(filePath)
		if os.IsNotExist(err) {
			continue // файл удалён – пропускаем
		}
		if err != nil {
			return err
		}
		entry.Size = uint32(info.Size())
		entry.ArchiveID = 1 // временно
		activeEntries = append(activeEntries, entry)
	}

	// 2. Добавляем новые файлы из папки (которых нет в метаданных)
	existingNames := make(map[string]bool)
	for _, e := range meta.Entries {
		existingNames[e.Name] = true
	}
	entries, err := os.ReadDir(inDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "_metadata.json" {
			continue
		}
		// Проверяем, есть ли уже такой файл среди записей
		found := false
		for _, entry := range meta.Entries {
			if safeNameFromBytes([]byte(entry.Name)) == e.Name() {
				found = true
				break
			}
		}
		if !found {
			info, err := e.Info()
			if err != nil {
				return err
			}
			newEntry := Entry{
				Name:      e.Name(), // безопасное имя, но оно будет записано в TOC как есть
				Size:      uint32(info.Size()),
				ArchiveID: 1,
				OffsetID:  0,
			}
			activeEntries = append(activeEntries, newEntry)
		}
	}

	// 3. Применяем релинк по имени (если включён) – перенаправляет OffsetID для дубликатов
	if relinkName {
		if err := relinkByName(activeEntries); err != nil {
			return err
		}
	}

	// 4. Применяем дедупликацию по содержимому (если включена)
	if dedup {
		hashToIdx := make(map[uint64]int)
		for i := range activeEntries {
			entry := &activeEntries[i]
			safeName := safeNameFromBytes([]byte(entry.Name))
			filePath := filepath.Join(inDir, safeName)
			hash, err := computeHashFile(filePath)
			if err != nil {
				return err
			}
			if idx, ok := hashToIdx[hash]; ok {
				// Дубликат – перенаправляем на оригинал
				entry.OffsetID = activeEntries[idx].OffsetID
				entry.ArchiveID = 1
			} else {
				hashToIdx[hash] = i
			}
		}
	}

	// 5. Пересчитываем OffsetID для уникальных данных (один OffsetID = один физический экземпляр)
	offsetMap := make(map[uint32]uint32)
	newOffsetID := uint32(0)
	for i := range activeEntries {
		if _, ok := offsetMap[activeEntries[i].OffsetID]; !ok {
			offsetMap[activeEntries[i].OffsetID] = newOffsetID
			newOffsetID++
		}
	}
	for i := range activeEntries {
		if newID, ok := offsetMap[activeEntries[i].OffsetID]; ok {
			activeEntries[i].OffsetID = newID
		} else {
			return fmt.Errorf("некорректный OffsetID у записи %d", i)
		}
	}

	// 6. Определяем уникальные индексы (те, у которых OffsetID встречается впервые)
	seenOffset := make(map[uint32]bool)
	var uniqueIndices []int
	for i, entry := range activeEntries {
		if !seenOffset[entry.OffsetID] {
			seenOffset[entry.OffsetID] = true
			uniqueIndices = append(uniqueIndices, i)
		}
	}

	// 7. Строим новый PAK, записывая только уникальные данные
	outPak, err := os.Create(outPakPath)
	if err != nil {
		return err
	}
	defer outPak.Close()

	newLbaTable := make([]uint32, len(uniqueIndices))
	for i, idx := range uniqueIndices {
		entry := activeEntries[idx]
		cur, _ := outPak.Seek(0, io.SeekCurrent)
		padding := (SectorSize - (cur % SectorSize)) % SectorSize
		if padding > 0 {
			outPak.Write(make([]byte, padding))
		}
		newPos, _ := outPak.Seek(0, io.SeekCurrent)
		newLba := uint32(newPos / SectorSize)
		newLbaTable[i] = newLba

		safeName := safeNameFromBytes([]byte(entry.Name))
		srcPath := filepath.Join(inDir, safeName)
		src, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		_, err = io.Copy(outPak, src)
		src.Close()
		if err != nil {
			return err
		}
	}

	// 8. Формируем новый TOC
	newFileCount := uint32(len(activeEntries))
	newTocData := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(newTocData, newFileCount)

	for _, entry := range activeEntries {
		nameBuf := make([]byte, NameLen)
		copy(nameBuf, []byte(entry.Name))
		newTocData = append(newTocData, nameBuf...)
		sizeBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(sizeBytes, entry.Size)
		newTocData = append(newTocData, sizeBytes...)
		arcBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(arcBytes, 1) // все в PART1
		newTocData = append(newTocData, arcBytes...)
		offBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(offBytes, entry.OffsetID)
		newTocData = append(newTocData, offBytes...)
	}

	if len(newTocData) < TocLbaOffset {
		padding := make([]byte, TocLbaOffset-len(newTocData))
		newTocData = append(newTocData, padding...)
	} else if len(newTocData) > TocLbaOffset {
		return fmt.Errorf("размер TOC превышает 0x8068")
	}

	// Строим полную таблицу LBA для всех записей
	fullLbaTable := make([]uint32, newFileCount)
	for i, entry := range activeEntries {
		if entry.OffsetID < uint32(len(newLbaTable)) {
			fullLbaTable[i] = newLbaTable[entry.OffsetID]
		} else {
			return fmt.Errorf("некорректный OffsetID %d у записи %d", entry.OffsetID, i)
		}
	}
	for _, lba := range fullLbaTable {
		lbaBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(lbaBytes, lba)
		newTocData = append(newTocData, lbaBytes...)
	}

	return os.WriteFile(outTocPath, newTocData, 0644)
}

// ---------- Вывод справки ----------

func printUsage() {
	fmt.Println(`God of War 2 Repacker

Использование:
  gow2repacker extract [toc] [pak1] [pak2] [out]
  gow2repacker pack [in] [outpak] [outtoc] [-dedup] [-relinkname]
  gow2repacker help

Примеры:
  gow2repacker extract
  gow2repacker extract GODOFWAR.TOC PART1.PAK PART2.PAK MY_EXTRACT
  gow2repacker pack -dedup
  gow2repacker pack MY_EXTRACT NEW_PART1.PAK NEW_TOC.TOC -relinkname=false
`)
}

// ---------- MAIN ----------

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printUsage()
		os.Exit(0)
	}

	switch args[0] {
	case "extract":
		tocPath := "GODOFWAR.TOC"
		pak1Path := "PART1.PAK"
		pak2Path := "PART2.PAK"
		outDir := "UNPAK"
		pos := 1
		if len(args) > pos {
			tocPath = args[pos]
			pos++
		}
		if len(args) > pos {
			pak1Path = args[pos]
			pos++
		}
		if len(args) > pos {
			pak2Path = args[pos]
			pos++
		}
		if len(args) > pos {
			outDir = args[pos]
		}
		meta, err := readTOC(tocPath, pak1Path, pak2Path)
		if err != nil {
			fmt.Printf("Ошибка: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Найдено файлов: %d\n", meta.FileCount)
		if err := extractFiles(meta, outDir); err != nil {
			fmt.Printf("Ошибка распаковки: %v\n", err)
			os.Exit(1)
		}
		if err := saveMetadata(meta, outDir); err != nil {
			fmt.Printf("Ошибка сохранения метаданных: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Распаковка завершена в %s\n", outDir)

	case "pack":
		inDir := "UNPAK"
		outPakPath := "PART1_NEW.PAK"
		outTocPath := "GODOFWAR_NEW.TOC"
		dedup := false
		relinkName := true
		var positional []string
		for _, arg := range args[1:] {
			if strings.HasPrefix(arg, "-") {
				parts := strings.SplitN(arg, "=", 2)
				if len(parts) == 2 {
					switch parts[0] {
					case "-dedup":
						dedup = parts[1] != "false"
					case "-relinkname":
						relinkName = parts[1] != "false"
					}
				} else {
					switch arg {
					case "-dedup":
						dedup = true
					case "-relinkname":
						relinkName = true
					}
				}
			} else {
				positional = append(positional, arg)
			}
		}
		if len(positional) > 0 {
			inDir = positional[0]
		}
		if len(positional) > 1 {
			outPakPath = positional[1]
		}
		if len(positional) > 2 {
			outTocPath = positional[2]
		}
		if err := pack(inDir, outPakPath, outTocPath, dedup, relinkName); err != nil {
			fmt.Printf("Ошибка упаковки: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Упаковка завершена. Созданы: %s и %s\n", outPakPath, outTocPath)

	default:
		printUsage()
	}
}
