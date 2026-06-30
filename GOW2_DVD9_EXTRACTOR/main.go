package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	GOW2_DVDDL_SPLITLINE = 10000000
	SECTOR_SIZE          = 2048
)

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

func main() {
	if len(os.Args) < 5 || os.Args[1] != "extract" {
		fmt.Println("Usage: extract <toc_file> <pak1_file> <pak2_file> <output_dir>")
		fmt.Println("       If PART2.PAK is missing, pass 'none' as the third file.")
		return
	}

	tocPath := os.Args[2]
	pak1Path := os.Args[3]
	pak2Path := os.Args[4]
	outputDir := os.Args[5]

	if err := extract(tocPath, pak1Path, pak2Path, outputDir); err != nil {
		fmt.Printf("Error: %v\n", err)
	}
}

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
	fmt.Printf("PART1.PAK size: %d bytes\n", pak1Size)

	var pak2 *os.File
	var pak2Size int64
	usePak2 := pak2Path != "" && strings.ToLower(pak2Path) != "none" && strings.ToLower(pak2Path) != "-"
	if usePak2 {
		pak2, err = os.Open(pak2Path)
		if err != nil {
			fmt.Printf("Warning: PART2.PAK not found, skipping files beyond PART1 size.\n")
		} else {
			defer pak2.Close()
			pak2Info, _ := pak2.Stat()
			pak2Size = pak2Info.Size()
			fmt.Printf("PART2.PAK size: %d bytes\n", pak2Size)
		}
	}

	os.MkdirAll(outputDir, 0755)

	extracted := 0
	skipped := 0

	for _, file := range files {
		var pakFile *os.File
		var offsetInPak int64
		var fileSizeLimit int64
		var pakName string
		var rawOffset = file.RawOffset
		var computedOffset int64

		// Логика из god_of_war_browser/drivers/toc/gow2.go
		if rawOffset >= GOW2_DVDDL_SPLITLINE {
			// Файл на втором слое (PART2.PAK)
			if pak2 == nil {
				fmt.Printf("Skipping %s (needs PART2.PAK)\n", file.Name)
				skipped++
				continue
			}
			pakFile = pak2
			computedOffset = int64(rawOffset%GOW2_DVDDL_SPLITLINE) * SECTOR_SIZE
			offsetInPak = computedOffset
			fileSizeLimit = pak2Size
			pakName = "PART2.PAK"
		} else {
			// Файл на первом слое (PART1.PAK)
			pakFile = pak1
			computedOffset = int64(rawOffset) * SECTOR_SIZE
			offsetInPak = computedOffset
			fileSizeLimit = pak1Size
			pakName = "PART1.PAK"
		}

		if offsetInPak+int64(file.Size) > fileSizeLimit {
			fmt.Printf("Warning: %s offset %d + size %d exceeds %s size %d, skipping\n",
				file.Name, offsetInPak, file.Size, pakName, fileSizeLimit)
			skipped++
			continue
		}

		data := make([]byte, file.Size)
		if _, err := pakFile.ReadAt(data, offsetInPak); err != nil {
			fmt.Printf("Error reading %s at offset %d: %v, skipping\n", file.Name, offsetInPak, err)
			skipped++
			continue
		}

		outPath := filepath.Join(outputDir, file.Name)
		os.MkdirAll(filepath.Dir(outPath), 0755)
		if err := os.WriteFile(outPath, data, 0644); err != nil {
			fmt.Printf("Error writing %s: %v, skipping\n", outPath, err)
			skipped++
			continue
		}

		fmt.Printf("Extracted: %s (%d bytes) from %s, rawOffset=%d, computedOffset=%d\n",
			file.Name, file.Size, pakName, rawOffset, computedOffset)
		extracted++
	}

	fmt.Printf("\nDone. Extracted %d files, skipped %d.\n", extracted, skipped)
	return nil
}

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
		e.Size = binary.LittleEndian.Uint32(data[offset+24:offset+28])
		e.ArchiveID = binary.LittleEndian.Uint32(data[offset+28:offset+32])
		e.OffsetID = binary.LittleEndian.Uint32(data[offset+32:offset+36])
		entries = append(entries, e)
		offset += 36
	}
	offsetTableStart := 4 + int(count)*36
	return entries, offsetTableStart, nil
}

func getFileInfos(tocData []byte, entries []TOCEntry, offsetTableStart int) []FileInfo {
	files := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		name := strings.TrimRight(string(e.Filename[:]), "\x00")
		ptrOffset := offsetTableStart + int(e.OffsetID)*4
		if ptrOffset+4 > len(tocData) {
			continue
		}
		raw := binary.LittleEndian.Uint32(tocData[ptrOffset:ptrOffset+4])
		files = append(files, FileInfo{
			Name:      name,
			Size:      e.Size,
			RawOffset: raw,
		})
	}
	return files
}
