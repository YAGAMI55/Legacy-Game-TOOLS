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
	"strings"
)

// ---------- Константы ----------
const (
	SectorSize   = 2048
	TocLbaOffset = 0x8068
	EntrySize    = 36
	NameLen      = 24
	HeaderSize   = 4
	SPLITLINE    = 10000000 // для DVD9
)

// ---------- Структуры ----------
type TOCEntry struct {
	Filename  [24]byte
	Size      uint32
	ArchiveID uint32
	OffsetID  uint32
}

type FileInfo struct {
	Name      string
	Size      uint32
	RawOffset uint32
}

type Entry struct {
	Name           string `json:"name"`
	Size           uint32 `json:"size"`
	ArchiveID      uint32 `json:"archiveId"`
	OffsetID       uint32 `json:"offsetId"`
	Lba            uint32 `json:"lba"`
	Pak            string `json:"pak"`
	Offset         int64  `json:"offset"`
	ComputedOffset int64  `json:"computedOffset"`
}

type Metadata struct {
	FileCount uint32  `json:"fileCount"`
	Entries   []Entry `json:"entries"`
}

// ---------- Инициализация логгера (без даты и времени) ----------
func init() {
	log.SetFlags(0)
}

// ---------- Вспомогательные ----------
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

// ---------- Парсинг TOC ----------
func parseTOC(data []byte) ([]TOCEntry, int, error) {
	if len(data) < 4 {
		return nil, 0, fmt.Errorf("TOC too small")
	}
	count := binary.LittleEndian.Uint32(data[0:4])
	entries := make([]TOCEntry, 0, count)
	offset := 4
	for i := uint32(0); i < count; i++ {
		if offset+36 > len(data) {
			return nil, 0, fmt.Errorf("truncated at entry %d", i)
		}
		var e TOCEntry
		copy(e.Filename[:], data[offset:offset+24])
		e.Size = binary.LittleEndian.Uint32(data[offset+24 : offset+28])
		e.ArchiveID = binary.LittleEndian.Uint32(data[offset+28 : offset+32])
		e.OffsetID = binary.LittleEndian.Uint32(data[offset+32 : offset+36])
		entries = append(entries, e)
		offset += 36
	}
	offsetTableStart := 4 + int(count)*36
	return entries, offsetTableStart, nil
}

// ---------- Получение информации о файлах ----------
func getFileInfos(tocData []byte, entries []TOCEntry, offsetTableStart int) []FileInfo {
	files := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		name := strings.TrimRight(string(e.Filename[:]), "\x00")
		ptrOffset := offsetTableStart + int(e.OffsetID)*4
		if ptrOffset+4 > len(tocData) {
			continue
		}
		raw := binary.LittleEndian.Uint32(tocData[ptrOffset : ptrOffset+4])
		files = append(files, FileInfo{
			Name:      name,
			Size:      e.Size,
			RawOffset: raw,
		})
	}
	return files
}

// ---------- Распаковка ----------
func extract(tocPath, pak1Path, pak2Path, outputDir string) error {
	tocData, err := os.ReadFile(tocPath)
	if err != nil {
		return fmt.Errorf("cannot read TOC: %w", err)
	}
	entries, offsetTableStart, err := parseTOC(tocData)
	if err != nil {
		return fmt.Errorf("parse error: %w", err)
	}
	files := getFileInfos(tocData, entries, offsetTableStart)

	pak1, err := os.Open(pak1Path)
	if err != nil {
		return fmt.Errorf("cannot open PART1.PAK: %w", err)
	}
	defer pak1.Close()
	pak1Info, _ := pak1.Stat()
	pak1Size := pak1Info.Size()
	log.Printf("PART1.PAK size: %d bytes", pak1Size)

	var pak2 *os.File
	var pak2Size int64
	usePak2 := pak2Path != "" && strings.ToLower(pak2Path) != "none" && strings.ToLower(pak2Path) != "-"
	if usePak2 {
		pak2, err = os.Open(pak2Path)
		if err != nil {
			log.Printf("Warning: PART2.PAK not found, skipping files beyond PART1 size.")
		} else {
			defer pak2.Close()
			pak2Info, _ := pak2.Stat()
			pak2Size = pak2Info.Size()
			log.Printf("PART2.PAK size: %d bytes", pak2Size)
		}
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return err
	}

	extracted := 0
	skipped := 0

	nameToEntry := make(map[string]TOCEntry)
	for _, e := range entries {
		name := strings.TrimRight(string(e.Filename[:]), "\x00")
		nameToEntry[name] = e
	}

	meta := &Metadata{
		FileCount: uint32(len(files)),
		Entries:   make([]Entry, 0, len(files)),
	}

	for i, file := range files {
		var pakFile *os.File
		var offsetInPak int64
		var fileSizeLimit int64
		var pakName string
		var rawOffset = file.RawOffset
		var computedOffset int64

		if rawOffset >= SPLITLINE {
			if pak2 == nil {
				log.Printf("Skipping %s (needs PART2.PAK)", file.Name)
				skipped++
				continue
			}
			pakFile = pak2
			computedOffset = int64(rawOffset%SPLITLINE) * SectorSize
			offsetInPak = computedOffset
			fileSizeLimit = pak2Size
			pakName = "PART2.PAK"
		} else {
			pakFile = pak1
			computedOffset = int64(rawOffset) * SectorSize
			offsetInPak = computedOffset
			fileSizeLimit = pak1Size
			pakName = "PART1.PAK"
		}

		if offsetInPak+int64(file.Size) > fileSizeLimit {
			log.Printf("Warning: %s offset %d + size %d exceeds %s size %d, skipping",
				file.Name, offsetInPak, file.Size, pakName, fileSizeLimit)
			skipped++
			continue
		}

		data := make([]byte, file.Size)
		if _, err := pakFile.ReadAt(data, offsetInPak); err != nil {
			log.Printf("Error reading %s at offset %d: %v, skipping", file.Name, offsetInPak, err)
			skipped++
			continue
		}

		safeName := safeNameFromBytes([]byte(file.Name))
		outPath := filepath.Join(outputDir, safeName)
		if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(outPath, data, 0644); err != nil {
			log.Printf("Error writing %s: %v, skipping", outPath, err)
			skipped++
			continue
		}

		log.Printf("[%d/%d] Распаковано: %s (%d bytes) from %s, rawOffset=%d, computedOffset=%d",
			i+1, len(files), file.Name, file.Size, pakName, rawOffset, computedOffset)
		extracted++

		e := nameToEntry[file.Name]
		meta.Entries = append(meta.Entries, Entry{
			Name:           file.Name,
			Size:           file.Size,
			ArchiveID:      e.ArchiveID,
			OffsetID:       e.OffsetID,
			Lba:            rawOffset,
			Pak:            pakName,
			Offset:         offsetInPak,
			ComputedOffset: computedOffset,
		})
	}

	log.Printf("Done. Extracted %d files, skipped %d.", extracted, skipped)

	metaPath := filepath.Join(outputDir, "_metadata.json")
	metaData, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(metaPath, metaData, 0644); err != nil {
		return fmt.Errorf("cannot save metadata: %w", err)
	}
	log.Printf("Metadata saved to %s", metaPath)

	return nil
}

// ---------- Упаковка (с релинком .psw -> .pss и дедупликацией) ----------
func pack(inDir, outPak, outToc string, relinkPss bool, dedup bool) error {
	log.Print("Начало упаковки...")

	metaPath := filepath.Join(inDir, "_metadata.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("cannot read metadata: %w", err)
	}
	var meta Metadata
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return fmt.Errorf("cannot parse metadata: %w", err)
	}
	log.Printf("Метаданные загружены, файлов: %d", meta.FileCount)

	type FileInfoPack struct {
		Entry  Entry
		Path   string
		Size   int64
		Exists bool
	}
	var fileInfos []FileInfoPack
	for _, entry := range meta.Entries {
		safeName := safeNameFromBytes([]byte(entry.Name))
		filePath := filepath.Join(inDir, safeName)
		info, err := os.Stat(filePath)
		if os.IsNotExist(err) {
			log.Printf("Файл %s удалён, пропускаем", entry.Name)
			continue
		}
		if err != nil {
			return err
		}
		fileInfos = append(fileInfos, FileInfoPack{
			Entry:  entry,
			Path:   filePath,
			Size:   info.Size(),
			Exists: true,
		})
	}
	if len(fileInfos) == 0 {
		return fmt.Errorf("нет файлов для упаковки")
	}
	log.Printf("Активных файлов: %d", len(fileInfos))

	relinkMap := make(map[int]int) // дубликат -> оригинал

	// 1. Релинк по имени (.psw -> .pss)
	if relinkPss {
		log.Print("Включён релинк по имени (.psw -> .pss)")
		groups := make(map[string][]int)
		for i, fi := range fileInfos {
			base := strings.TrimSuffix(fi.Entry.Name, filepath.Ext(fi.Entry.Name))
			groups[base] = append(groups[base], i)
		}
		for _, indices := range groups {
			if len(indices) <= 1 {
				continue
			}
			bestIdx := indices[0]
			bestPriority := -1
			for _, idx := range indices {
				ext := strings.ToLower(filepath.Ext(fileInfos[idx].Entry.Name))
				priority := 0
				if ext == ".pss" {
					priority = 2
				} else if ext == ".psw" {
					priority = 1
				}
				if priority > bestPriority {
					bestPriority = priority
					bestIdx = idx
				}
			}
			for _, idx := range indices {
				if idx != bestIdx {
					relinkMap[idx] = bestIdx
					log.Printf("Релинк: %s -> %s", fileInfos[idx].Entry.Name, fileInfos[bestIdx].Entry.Name)
				}
			}
		}
	}

	// 2. Дедупликация по содержимому (CRC64) – исключаем .pss и .psw
	if dedup {
		log.Print("Включена дедупликация по содержимому (CRC64)")
		hashToOrig := make(map[uint64]int)
		table := crc64.MakeTable(crc64.ECMA)

		for i, fi := range fileInfos {
			// Если файл уже дубликат по релинку, пропускаем
			if _, ok := relinkMap[i]; ok {
				continue
			}
			// Исключаем .pss и .psw
			ext := strings.ToLower(filepath.Ext(fi.Entry.Name))
			if ext == ".pss" || ext == ".psw" {
				continue
			}
			// Вычисляем CRC64
			f, err := os.Open(fi.Path)
			if err != nil {
				return err
			}
			h := crc64.New(table)
			if _, err := io.Copy(h, f); err != nil {
				f.Close()
				return err
			}
			f.Close()
			hash := h.Sum64()

			if origIdx, ok := hashToOrig[hash]; ok {
				// Найден дубликат
				relinkMap[i] = origIdx
				log.Printf("Дедупликация: %s -> %s (CRC64=%x)", fi.Entry.Name, fileInfos[origIdx].Entry.Name, hash)
			} else {
				hashToOrig[hash] = i
			}
		}
	}

	// 3. Определяем уникальные файлы (оригиналы)
	uniqueIndices := []int{}
	seen := make(map[int]bool)
	for i := range fileInfos {
		orig := i
		if val, ok := relinkMap[i]; ok {
			orig = val
		}
		if !seen[orig] {
			seen[orig] = true
			uniqueIndices = append(uniqueIndices, orig)
		}
	}
	log.Printf("Уникальных файлов для записи в PAK: %d", len(uniqueIndices))

	// 4. Создаём PAK
	out, err := os.Create(outPak)
	if err != nil {
		return err
	}
	defer out.Close()

	offsetToLba := make(map[uint32]uint32)
	currentPos := int64(0)
	for _, idx := range uniqueIndices {
		fi := fileInfos[idx]
		if currentPos%SectorSize != 0 {
			pad := SectorSize - (currentPos % SectorSize)
			if _, err := out.Write(make([]byte, pad)); err != nil {
				return err
			}
			currentPos += pad
		}
		lba := uint32(currentPos / SectorSize)
		offsetToLba[fi.Entry.OffsetID] = lba

		src, err := os.Open(fi.Path)
		if err != nil {
			return err
		}
		written, err := io.Copy(out, src)
		src.Close()
		if err != nil {
			return err
		}
		currentPos += written
		log.Printf("Упаковано: %s (LBA=%d, размер=%d)", fi.Entry.Name, lba, fi.Size)
	}

	// 5. Подготовка размеров для дубликатов
	actualSizeMap := make(map[int]int64)
	for i, fi := range fileInfos {
		if val, ok := relinkMap[i]; ok {
			actualSizeMap[i] = fileInfos[val].Size
			log.Printf("Для дубликата %s установлен размер оригинала %d", fi.Entry.Name, actualSizeMap[i])
		} else {
			actualSizeMap[i] = fi.Size
		}
	}

	// 6. Формируем TOC
	newFileCount := uint32(len(fileInfos))
	tocData := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(tocData, newFileCount)

	for i, fi := range fileInfos {
		nameBytes := make([]byte, NameLen)
		copy(nameBytes, []byte(fi.Entry.Name))
		tocData = append(tocData, nameBytes...)
		sizeBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(sizeBytes, uint32(actualSizeMap[i]))
		tocData = append(tocData, sizeBytes...)
		arcBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(arcBytes, 1)
		tocData = append(tocData, arcBytes...)
		offBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(offBytes, fi.Entry.OffsetID)
		tocData = append(tocData, offBytes...)
	}

	if len(tocData) < TocLbaOffset {
		pad := make([]byte, TocLbaOffset-len(tocData))
		tocData = append(tocData, pad...)
	} else if len(tocData) > TocLbaOffset {
		return fmt.Errorf("размер TOC превышает 0x8068")
	}

	// Таблица LBA – для дубликатов используем LBA оригинала
	for i, fi := range fileInfos {
		var targetOffsetID uint32
		if val, ok := relinkMap[i]; ok {
			targetOffsetID = fileInfos[val].Entry.OffsetID
		} else {
			targetOffsetID = fi.Entry.OffsetID
		}
		lba, ok := offsetToLba[targetOffsetID]
		if !ok {
			return fmt.Errorf("не найден LBA для OffsetID %d (исходный %d)", targetOffsetID, fi.Entry.OffsetID)
		}
		lbaBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(lbaBytes, lba)
		tocData = append(tocData, lbaBytes...)
	}

	if err := os.WriteFile(outToc, tocData, 0644); err != nil {
		return err
	}
	log.Printf("TOC записан: %s", outToc)
	log.Print("Упаковка завершена.")
	return nil
}

// ---------- Справка ----------
func printUsage() {
	fmt.Println(`GOW2 DVD9/DVD5 Extractor & Repacker by YAGAMI55

Использование:
  extract <GODOFWAR.TOC> <PART1.PAK> [PART2.PAK or 'none'] <OUT_DIR>
  pack <IN_DIR> <OUT_PAK> <OUT_TOC> [-relinkpss] [-dedup]
  help

Примеры:
  # Распаковка DVD5 (только PART1)
  gow2repacker extract GODOFWAR.TOC PART1.PAK none EXTRACT

  # Распаковка DVD9 (PART1 + PART2)
  gow2repacker extract GODOFWAR.TOC PART1.PAK PART2.PAK EXTRACT

  # Упаковка с релинком .psw -> .pss
  gow2repacker pack EXTRACT PART1_NEW.PAK GODOFWAR_NEW.TOC -relinkpss

  # Упаковка с релинком и дедупликацией (CRC64, исключая .pss/.psw)
  gow2repacker pack EXTRACT PART1_NEW.PAK GODOFWAR_NEW.TOC -relinkpss -dedup

Примечания:
  - При распаковке автоматически определяется DVD9 (если передан PART2) или DVD5.
  - Упаковка всегда создаёт один PART1.PAK (все ArchiveID=1).
  - Релинк (.psw -> .pss) работает по имени без расширения, приоритет .pss > .psw.
  - Дедупликация объединяет одинаковые файлы по CRC64, но не затрагивает .pss и .psw.
`)
}

// ---------- MAIN ----------
func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "help" {
		printUsage()
		os.Exit(0)
	}

	switch args[0] {
	case "extract":
		if len(args) < 5 {
			fmt.Println("Ошибка: недостаточно аргументов для extract")
			printUsage()
			os.Exit(1)
		}
		tocPath := args[1]
		pak1Path := args[2]
		pak2Path := args[3]
		outDir := args[4]
		if err := extract(tocPath, pak1Path, pak2Path, outDir); err != nil {
			log.Fatalf("Ошибка: %v", err)
		}

	case "pack":
		if len(args) < 4 {
			fmt.Println("Ошибка: недостаточно аргументов для pack")
			printUsage()
			os.Exit(1)
		}
		inDir := args[1]
		outPak := args[2]
		outToc := args[3]
		relinkPss := false
		dedup := false
		for _, arg := range args[4:] {
			if arg == "-relinkpss" {
				relinkPss = true
			}
			if arg == "-dedup" {
				dedup = true
			}
		}
		if err := pack(inDir, outPak, outToc, relinkPss, dedup); err != nil {
			log.Fatalf("Ошибка упаковки: %v", err)
		}

	default:
		printUsage()
		os.Exit(1)
	}
}