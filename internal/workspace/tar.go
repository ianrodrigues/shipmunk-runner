package workspace

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	defaultMaxFiles = 20_000
	defaultMaxBytes = 128 * 1024 * 1024
	tarBlockSize    = 512
)

// TarExtractor accepts ordinary tar and gzip-compressed tar while rejecting
// links, special files, unsafe paths, and unbounded archives.
type TarExtractor struct {
	MaxFiles int
	MaxBytes int64
}

// Extract expands archive into an existing real directory.
func (extractor TarExtractor) Extract(ctx context.Context, archive []byte, destination string) error {
	maxFiles, maxBytes := extractor.limits()
	decoded, err := decompress(ctx, archive, maxBytes)
	if err != nil {
		return err
	}
	info, err := os.Lstat(destination)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("extraction destination must be a real directory")
	}

	offset := 0
	files := 0
	var totalBytes int64
	var pendingPath *string
	for offset+tarBlockSize <= len(decoded) {
		if err := ctx.Err(); err != nil {
			return err
		}
		header := decoded[offset : offset+tarBlockSize]
		offset += tarBlockSize
		if allZero(header) {
			if pendingPath != nil {
				return fmt.Errorf("archive contains an unused path extension")
			}
			return nil
		}
		if err := verifyChecksum(header); err != nil {
			return err
		}
		size, err := octal(header[124:136])
		if err != nil {
			return err
		}
		typeFlag := header[156]
		files++
		if files > maxFiles || size > maxBytes-totalBytes {
			return fmt.Errorf("archive exceeded extraction limits")
		}
		totalBytes += size
		if size > int64(len(decoded)-offset) {
			return fmt.Errorf("archive entry is truncated")
		}
		contentEnd := offset + int(size)
		paddedEnd := offset + paddedSize(size)
		if paddedEnd > len(decoded) {
			return fmt.Errorf("archive entry is truncated")
		}
		content := decoded[offset:contentEnd]
		if typeFlag == 'g' || typeFlag == 'x' {
			if pendingPath != nil || size > 65_536 {
				return fmt.Errorf("archive path extension is invalid")
			}
			if _, err := archivePath(header, typeFlag, nil); err != nil {
				return err
			}
			attributes, err := paxAttributes(content, typeFlag)
			if err != nil {
				return err
			}
			if path, ok := attributes["path"]; ok {
				pendingPath = &path
			}
			offset = paddedEnd
			continue
		}

		name, err := archivePath(header, typeFlag, pendingPath)
		if err != nil {
			return err
		}
		pendingPath = nil
		target := destination
		for _, segment := range strings.Split(name, "/") {
			target = filepath.Join(target, segment)
		}
		switch typeFlag {
		case '5':
			if err := makeDirectory(destination, target); err != nil {
				return err
			}
		case 0, '0':
			if err := makeDirectory(destination, filepath.Dir(target)); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return fmt.Errorf("unable to extract archive entry: %w", err)
			}
			if _, err := io.Copy(file, bytes.NewReader(content)); err != nil {
				_ = file.Close()
				return fmt.Errorf("unable to extract archive entry: %w", err)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("unable to extract archive entry: %w", err)
			}
			mode, err := octal(header[100:108])
			if err != nil {
				return err
			}
			permissions := os.FileMode(0600)
			if mode&0100 != 0 {
				permissions = 0700
			}
			if err := os.Chmod(target, permissions); err != nil {
				return fmt.Errorf("unable to set archive file permissions: %w", err)
			}
		default:
			return fmt.Errorf("archive links and special files are forbidden")
		}
		offset = paddedEnd
	}
	return fmt.Errorf("archive is missing its end marker")
}

func (extractor TarExtractor) limits() (int, int64) {
	maxFiles := extractor.MaxFiles
	if maxFiles <= 0 {
		maxFiles = defaultMaxFiles
	}
	maxBytes := extractor.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	return maxFiles, maxBytes
}

func decompress(ctx context.Context, archive []byte, maxBytes int64) ([]byte, error) {
	if len(archive) < 2 || archive[0] != 0x1f || archive[1] != 0x8b {
		if int64(len(archive)) > maxBytes {
			return nil, fmt.Errorf("archive exceeded extraction limits")
		}
		return archive, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("unable to initialize gzip extraction: %w", err)
	}
	defer reader.Close()

	var decoded bytes.Buffer
	buffer := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			if int64(decoded.Len())+int64(count) > maxBytes {
				return nil, fmt.Errorf("compressed archive exceeded extraction limits")
			}
			if _, err := decoded.Write(buffer[:count]); err != nil {
				return nil, fmt.Errorf("unable to decompress archive")
			}
		}
		if readErr == io.EOF {
			return decoded.Bytes(), nil
		}
		if readErr != nil {
			return nil, fmt.Errorf("compressed archive is invalid: %w", readErr)
		}
	}
}

func archivePath(header []byte, typeFlag byte, override *string) (string, error) {
	name := strings.TrimRight(string(header[0:100]), "\x00")
	prefix := strings.TrimRight(string(header[345:500]), "\x00")
	path := name
	if prefix != "" {
		path = prefix + "/" + name
	}
	if override != nil {
		path = *override
	}
	if typeFlag == '5' && strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}
	if path == "" || strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\\\x00") || !utf8.ValidString(path) {
		return "", fmt.Errorf("archive path is unsafe")
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("archive path traversal is forbidden")
		}
	}
	return path, nil
}

var paxLengthPattern = regexp.MustCompile(`^[1-9][0-9]{0,5}$`)

func paxAttributes(payload []byte, typeFlag byte) (map[string]string, error) {
	attributes := make(map[string]string)
	allowed := "path"
	if typeFlag == 'g' {
		allowed = "comment"
	}
	for offset := 0; offset < len(payload); {
		space := bytes.IndexByte(payload[offset:], ' ')
		if space <= 0 || !paxLengthPattern.Match(payload[offset:offset+space]) {
			return nil, fmt.Errorf("archive extension record length is invalid")
		}
		length, err := strconv.Atoi(string(payload[offset : offset+space]))
		if err != nil || length <= space+2 || length > len(payload)-offset || payload[offset+length-1] != '\n' {
			return nil, fmt.Errorf("archive extension record is truncated")
		}
		record := payload[offset+space+1 : offset+length-1]
		separator := bytes.IndexByte(record, '=')
		if separator < 0 {
			return nil, fmt.Errorf("archive extension record is invalid")
		}
		key := string(record[:separator])
		if key != allowed {
			return nil, fmt.Errorf("archive extension attribute is unsupported")
		}
		if _, exists := attributes[key]; exists {
			return nil, fmt.Errorf("archive extension attribute is unsupported")
		}
		value := string(record[separator+1:])
		if !utf8.ValidString(value) {
			return nil, fmt.Errorf("archive extension record is invalid")
		}
		attributes[key] = value
		offset += length
	}
	if typeFlag == 'x' {
		if _, ok := attributes["path"]; !ok {
			return nil, fmt.Errorf("archive path extension is missing its path")
		}
	}
	return attributes, nil
}

func makeDirectory(destination, directory string) error {
	relative, err := filepath.Rel(destination, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("archive path is unsafe")
	}
	current := destination
	if relative == "." {
		return nil
	}
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0700); err != nil && !os.IsExist(err) {
				return fmt.Errorf("unable to create archive directory: %w", err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive directory is a symbolic link or not a directory")
		}
		if err := os.Chmod(current, 0700); err != nil {
			return fmt.Errorf("unable to set archive directory permissions: %w", err)
		}
	}
	return nil
}

func verifyChecksum(header []byte) error {
	expected, err := octal(header[148:156])
	if err != nil {
		return fmt.Errorf("archive header checksum is invalid")
	}
	var checksum int64
	for index, value := range header {
		if index >= 148 && index < 156 {
			checksum += int64(' ')
		} else {
			checksum += int64(value)
		}
	}
	if checksum != expected {
		return fmt.Errorf("archive header checksum is invalid")
	}
	return nil
}

func octal(value []byte) (int64, error) {
	trimmed := strings.Trim(string(value), " \x00")
	if trimmed == "" {
		return 0, nil
	}
	for _, digit := range trimmed {
		if digit < '0' || digit > '7' {
			return 0, fmt.Errorf("archive size is invalid")
		}
	}
	parsed, err := strconv.ParseInt(trimmed, 8, 63)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("archive size is invalid")
	}
	return parsed, nil
}

func paddedSize(size int64) int {
	return int((size + tarBlockSize - 1) / tarBlockSize * tarBlockSize)
}

func allZero(block []byte) bool {
	for _, value := range block {
		if value != 0 {
			return false
		}
	}
	return true
}

func unwrapGitHubSnapshot(ctx context.Context, directory string, revision any) error {
	sha, ok := revision.(string)
	if !ok || !regexp.MustCompile(`(?i)^[0-9a-f]{40}$`).MatchString(sha) {
		return nil
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("unable to inspect the source snapshot: %w", err)
	}
	if len(entries) != 1 {
		return nil
	}
	wrapper := entries[0]
	suffix := strings.ToLower(sha[:7])
	name := wrapper.Name()
	lowerName := strings.ToLower(name)
	shortSuffix := "-" + suffix
	fullSuffix := "-" + strings.ToLower(sha)
	if (!(strings.HasSuffix(lowerName, shortSuffix) && len(lowerName) > len(shortSuffix)) && !(strings.HasSuffix(lowerName, fullSuffix) && len(lowerName) > len(fullSuffix))) || !wrapper.IsDir() || wrapper.Type()&os.ModeSymlink != 0 {
		return nil
	}
	source := filepath.Join(directory, name)
	staging := directory + ".archive-root"
	if _, err := os.Lstat(staging); err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("archive normalization destination already exists")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(source, staging); err != nil {
		return fmt.Errorf("unable to stage the source archive root: %w", err)
	}
	children, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("unable to inspect the source archive root: %w", err)
	}
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return err
		}
		destination := filepath.Join(directory, child.Name())
		if _, err := os.Lstat(destination); err == nil || !os.IsNotExist(err) {
			return fmt.Errorf("unable to normalize the source archive root")
		}
		if err := os.Rename(filepath.Join(staging, child.Name()), destination); err != nil {
			return fmt.Errorf("unable to normalize the source archive root: %w", err)
		}
	}
	if err := os.Remove(staging); err != nil {
		return fmt.Errorf("unable to remove the source archive wrapper: %w", err)
	}
	return ctx.Err()
}
