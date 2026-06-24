package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	SectorSize   = 2048
	TocLbaOffset = 0x8068
	EntrySize    = 36
	NameLen      = 24
	HeaderSize   = 4
)

type Entry struct {
	Name      string `json:"name"`
	Size      uint32 `json:"size"`
	ArchiveID uint32 `json:"archiveId"`
	OffsetID  uint32 `json:"offsetId"`
	Lba       uint32 `json:"lba"`
	Pak       string `json:"pak"`
	Offset    int64  `json:"offset"`
}

type Metadata struct {
	FileCount uint32  `json:"fileCount"`
	Entries   []Entry `json:"entries"`
}

func printProgress(current, total int, prefix string) {
	if total == 0 {
		return
	}
	percent := int(float64(current) / float64(total) * 100)
	msg := fmt.Sprintf("%s: %d%% (%d/%d)", prefix, percent, current, total)
	log.Print(msg)
	fmt.Printf("\r%s", msg)
	os.Stdout.Sync()
	if current == total {
		fmt.Println()
	}
}

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

func readTOC(tocPath, pak1Path, pak2Path string) (*Metadata, error) {
	log.Print("Чтение TOC...")
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
	log.Printf("TOC прочитан, файлов: %d", fileCount)
	return meta, nil
}

func extractFiles(meta *Metadata, outDir string) error {
	log.Print("Начало распаковки файлов...")
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}
	handles := make(map[string]*os.File)
	defer func() {
		for _, f := range handles {
			f.Close()
		}
	}()

	total := len(meta.Entries)
	for i, entry := range meta.Entries {
		printProgress(i+1, total, "Распаковка")

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
	printProgress(total, total, "Распаковка")
	log.Print("Распаковка завершена.")
	return nil
}

func saveMetadata(meta *Metadata, outDir string) error {
	log.Print("Сохранение метаданных...")
	path := filepath.Join(outDir, "_metadata.json")
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func loadMetadata(inDir string) (*Metadata, error) {
	log.Print("Загрузка метаданных...")
	path := filepath.Join(inDir, "_metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	log.Printf("Метаданные загружены, файлов: %d", meta.FileCount)
	return &meta, nil
}

func computeHashFileWithProgress(path string, fileIndex, total int) (uint64, error) {
	printProgress(fileIndex+1, total, "Хеширование файлов")
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

func relinkByName(entries []Entry) (map[uint32]uint32, error) {
	log.Print("Выполнение релинка по имени...")
	priorityExts := []string{".pss", ".psw"}
	groups := make(map[string][]int)
	for i, entry := range entries {
		base := strings.ToLower(filepath.Base(entry.Name))
		ext := filepath.Ext(base)
		isPriority := false
		for _, p := range priorityExts {
			if ext == p {
				isPriority = true
				break
			}
		}
		var key string
		if isPriority {
			key = strings.TrimSuffix(base, ext)
		} else {
			key = base
		}
		groups[key] = append(groups[key], i)
	}

	relinkMap := make(map[uint32]uint32)
	for _, indices := range groups {
		if len(indices) <= 1 {
			continue
		}
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
		origOffsetID := entries[bestIdx].OffsetID
		for _, idx := range indices {
			if idx == bestIdx {
				continue
			}
			dupOffsetID := entries[idx].OffsetID
			relinkMap[dupOffsetID] = origOffsetID
		}
	}
	log.Printf("Релинг по имени завершён, найдено %d дубликатов", len(relinkMap))
	return relinkMap, nil
}

func pack(inDir, outPakPath, outTocPath string, dedup bool, relinkName bool) error {
	startTime := time.Now()
	log.Print("=== НАЧАЛО УПАКОВКИ ===")

	meta, err := loadMetadata(inDir)
	if err != nil {
		return fmt.Errorf("загрузка метаданных: %v", err)
	}

	log.Print("Сбор активных файлов...")
	var activeEntries []Entry
	for _, entry := range meta.Entries {
		safeName := safeNameFromBytes([]byte(entry.Name))
		filePath := filepath.Join(inDir, safeName)
		info, err := os.Stat(filePath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		entry.Size = uint32(info.Size())
		entry.ArchiveID = 1
		activeEntries = append(activeEntries, entry)
	}

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
				Name:      e.Name(),
				Size:      uint32(info.Size()),
				ArchiveID: 1,
				OffsetID:  0,
			}
			activeEntries = append(activeEntries, newEntry)
		}
	}
	log.Printf("Активных файлов: %d", len(activeEntries))

	relinkMap := make(map[uint32]uint32)
	if relinkName {
		relinkMap, err = relinkByName(activeEntries)
		if err != nil {
			return err
		}
	}

	dedupMap := make(map[uint32]uint32)
	if dedup {
		log.Print("Выполнение дедупликации (хеширование файлов)...")
		totalFiles := len(activeEntries)
		hashToIdx := make(map[uint64]int)
		for i := range activeEntries {
			entry := &activeEntries[i]
			safeName := safeNameFromBytes([]byte(entry.Name))
			filePath := filepath.Join(inDir, safeName)
			hash, err := computeHashFileWithProgress(filePath, i, totalFiles)
			if err != nil {
				return err
			}
			if idx, ok := hashToIdx[hash]; ok {
				origOffset := activeEntries[idx].OffsetID
				dupOffset := entry.OffsetID
				dedupMap[dupOffset] = origOffset
			} else {
				hashToIdx[hash] = i
			}
		}
		printProgress(totalFiles, totalFiles, "Хеширование файлов")
		log.Printf("Дедупликация завершена, найдено %d дубликатов", len(dedupMap))
	}

	finalRedirect := make(map[uint32]uint32)
	for dup, orig := range relinkMap {
		finalRedirect[dup] = orig
	}
	for dup, orig := range dedupMap {
		if redirected, ok := finalRedirect[orig]; ok {
			finalRedirect[dup] = redirected
		} else {
			finalRedirect[dup] = orig
		}
	}

	// Разрешение цепочек с защитой от циклов
	finalTarget := make(map[uint32]uint32)
	for _, entry := range activeEntries {
		origID := entry.OffsetID
		visited := make(map[uint32]bool)
		current := origID
		for {
			if visited[current] {
				// Обнаружен цикл – прерываем, оставляем current как конечный
				break
			}
			visited[current] = true
			if next, ok := finalRedirect[current]; ok {
				current = next
			} else {
				break
			}
		}
		finalTarget[origID] = current
	}

	uniqueOffsetIDsSet := make(map[uint32]bool)
	for _, target := range finalTarget {
		uniqueOffsetIDsSet[target] = true
	}
	var uniqueOffsets []uint32
	for off := range uniqueOffsetIDsSet {
		uniqueOffsets = append(uniqueOffsets, off)
	}
	sort.Slice(uniqueOffsets, func(i, j int) bool { return uniqueOffsets[i] < uniqueOffsets[j] })

	log.Printf("Уникальных файлов для записи в PAK: %d", len(uniqueOffsets))
	log.Print("Начинаем запись PAK...")

	outPak, err := os.Create(outPakPath)
	if err != nil {
		return fmt.Errorf("ошибка создания PAK: %v", err)
	}
	defer outPak.Close()

	offsetToLba := make(map[uint32]uint32)
	totalUnique := len(uniqueOffsets)

	for i, off := range uniqueOffsets {
		log.Printf("Цикл %d из %d: обработка OffsetID %d", i+1, totalUnique, off)
		printProgress(i+1, totalUnique, "Упаковка (запись в PAK)")

		var srcEntry *Entry
		for idx := range activeEntries {
			if activeEntries[idx].OffsetID == off {
				srcEntry = &activeEntries[idx]
				break
			}
		}
		if srcEntry == nil {
			return fmt.Errorf("не найден файл для OffsetID %d", off)
		}
		log.Printf("  Найдена запись: %s, размер %d", srcEntry.Name, srcEntry.Size)

		cur, _ := outPak.Seek(0, io.SeekCurrent)
		padding := (SectorSize - (cur % SectorSize)) % SectorSize
		if padding > 0 {
			log.Printf("  Добавление выравнивания %d байт", padding)
			if _, err := outPak.Write(make([]byte, padding)); err != nil {
				return fmt.Errorf("ошибка записи выравнивания: %v", err)
			}
		}
		newPos, err := outPak.Seek(0, io.SeekCurrent)
		if err != nil {
			return fmt.Errorf("ошибка поиска позиции: %v", err)
		}
		newLba := uint32(newPos / SectorSize)
		offsetToLba[off] = newLba
		log.Printf("  Новый LBA: %d (смещение %d)", newLba, newPos)

		safeName := safeNameFromBytes([]byte(srcEntry.Name))
		srcPath := filepath.Join(inDir, safeName)
		log.Printf("  Открытие файла: %s", srcPath)

		if _, err := os.Stat(srcPath); err != nil {
			return fmt.Errorf("файл %s не существует: %v", srcPath, err)
		}
		src, err := os.Open(srcPath)
		if err != nil {
			return fmt.Errorf("ошибка открытия %s: %v", srcPath, err)
		}
		log.Printf("  Копирование данных...")
		written, err := io.Copy(outPak, src)
		src.Close()
		if err != nil {
			return fmt.Errorf("ошибка копирования %s: %v", srcPath, err)
		}
		if written != int64(srcEntry.Size) {
			log.Printf("  Предупреждение: скопировано %d байт, ожидалось %d", written, srcEntry.Size)
		} else {
			log.Printf("  Скопировано %d байт", written)
		}
		log.Printf("  Файл %s успешно обработан", srcEntry.Name)
	}
	printProgress(totalUnique, totalUnique, "Упаковка (запись в PAK)")
	log.Print("Запись PAK завершена.")

	log.Print("Формирование TOC...")
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
		binary.LittleEndian.PutUint32(arcBytes, 1)
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

	fullLbaTable := make([]uint32, newFileCount)
	for i, entry := range activeEntries {
		targetOff := finalTarget[entry.OffsetID]
		if lba, ok := offsetToLba[targetOff]; ok {
			fullLbaTable[i] = lba
		} else {
			return fmt.Errorf("не найден LBA для конечного OffsetID %d (исходный %d)", targetOff, entry.OffsetID)
		}
	}
	for _, lba := range fullLbaTable {
		lbaBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(lbaBytes, lba)
		newTocData = append(newTocData, lbaBytes...)
	}

	log.Printf("Запись TOC в %s...", outTocPath)
	if err := os.WriteFile(outTocPath, newTocData, 0644); err != nil {
		return err
	}

	log.Printf("Упаковка завершена за %v", time.Since(startTime))
	return nil
}

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
			log.Fatalf("Ошибка: %v", err)
		}
		fmt.Printf("Найдено файлов: %d\n", meta.FileCount)
		if err := extractFiles(meta, outDir); err != nil {
			log.Fatalf("Ошибка распаковки: %v", err)
		}
		if err := saveMetadata(meta, outDir); err != nil {
			log.Fatalf("Ошибка сохранения метаданных: %v", err)
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
			log.Fatalf("Ошибка упаковки: %v", err)
		}
		fmt.Printf("Упаковка завершена. Созданы: %s и %s\n", outPakPath, outTocPath)

	default:
		printUsage()
	}
}