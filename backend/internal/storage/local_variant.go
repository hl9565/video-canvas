package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// ErrSourceChanged 表示生成期间原文件被替换，应针对新指纹重试。
var ErrSourceChanged = errors.New("storage: 预览原文件已变化")

// LocalSourceInfo 包含本地原文件路径和缓存指纹。零值不表示有效文件。
type LocalSourceInfo struct {
	Path      string
	Size      int64
	MTimeNano int64
}

// RootDir 返回绝对存储目录，用于区分不同本地存储的生成任务。
func (l *LocalStorage) RootDir() string { return l.dir }

// StatSource 检查本地原文件；非法 key、目录和不存在的文件均返回 ErrNotFound。
func (l *LocalStorage) StatSource(key string) (LocalSourceInfo, error) {
	full, err := l.path(key)
	if err != nil {
		return LocalSourceInfo{}, ErrNotFound
	}
	info, err := os.Stat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return LocalSourceInfo{}, ErrNotFound
	}
	if err != nil {
		return LocalSourceInfo{}, fmt.Errorf("storage: 读取原文件信息失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return LocalSourceInfo{}, ErrNotFound
	}
	return LocalSourceInfo{Path: full, Size: info.Size(), MTimeNano: info.ModTime().UnixNano()}, nil
}

// LookupVariant 只命中当前原文件指纹对应的非空缓存文件。
func (l *LocalStorage) LookupVariant(key, variant, ext string) (*LocalFile, bool, error) {
	lock := l.keyLock(key)
	lock.Lock()
	defer lock.Unlock()
	source, err := l.StatSource(key)
	if err != nil {
		return nil, false, err
	}
	file, err := l.variantFile(key, variant, ext, source)
	if err != nil {
		return nil, false, err
	}
	info, err := os.Stat(file.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("storage: 读取变体缓存失败: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, false, nil
	}
	return file, true, nil
}

// PublishVariant 先写临时文件，再复核原文件并原子发布。
func (l *LocalStorage) PublishVariant(key, variant, ext string, source LocalSourceInfo, writeFn func(io.Writer) error) (*LocalFile, error) {
	return l.publishVariant(key, variant, ext, source, writeFn, func() error { return nil })
}

// PublishVariantContext 也在发布的临界区中尊重取消，避免超时的生成任务写入缓存。
func (l *LocalStorage) PublishVariantContext(ctx context.Context, key, variant, ext string, source LocalSourceInfo, writeFn func(io.Writer) error) (*LocalFile, error) {
	return l.publishVariant(key, variant, ext, source, writeFn, ctx.Err)
}

func (l *LocalStorage) publishVariant(key, variant, ext string, source LocalSourceInfo, writeFn func(io.Writer) error, checkCanceled func() error) (*LocalFile, error) {
	if err := checkCanceled(); err != nil {
		return nil, err
	}
	file, err := l.variantFile(key, variant, ext, source)
	if err != nil {
		return nil, err
	}
	tmp, err := l.stageVariant(key, file, source)
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := writeFn(tmp); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	info, statErr := tmp.Stat()
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("storage: 关闭变体临时文件失败: %w", err)
	}
	if statErr != nil {
		return nil, fmt.Errorf("storage: 检查变体临时文件失败: %w", statErr)
	}
	if info.Size() == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	lock := l.keyLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := l.verifyVariantSource(key, source); err != nil {
		return nil, err
	}
	if err := checkCanceled(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, file.path); err != nil {
		return nil, fmt.Errorf("storage: 发布变体失败: %w", err)
	}
	return file, nil
}

func (l *LocalStorage) stageVariant(key string, file *LocalFile, source LocalSourceInfo) (*os.File, error) {
	lock := l.keyLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := l.verifyVariantSource(key, source); err != nil {
		return nil, err
	}
	dir := filepath.Dir(file.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("storage: 创建变体目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("storage: 创建变体临时文件失败: %w", err)
	}
	return tmp, nil
}

func (l *LocalStorage) verifyVariantSource(key string, source LocalSourceInfo) error {
	current, err := l.StatSource(key)
	if err != nil {
		return err
	}
	if source != current {
		return ErrSourceChanged
	}
	return nil
}

func (l *LocalStorage) variantFile(key, variant, ext string, source LocalSourceInfo) (*LocalFile, error) {
	if variant != "thumb" && variant != "poster" {
		return nil, ErrInvalidKey
	}
	var contentType string
	switch ext {
	case "webp":
		contentType = "image/webp"
	case "jpeg":
		contentType = "image/jpeg"
	default:
		return nil, ErrInvalidKey
	}
	name := variant + "." + ext
	fingerprint := fmt.Sprintf("%d-%d", source.Size, source.MTimeNano)
	return &LocalFile{path: filepath.Join(l.variantDir(key), "p2", fingerprint, name), name: name, ContentType: contentType, variant: true}, nil
}

func (l *LocalStorage) variantDir(key string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	return filepath.Join(l.dir, ".variants", hash[:2], hash)
}

func (l *LocalStorage) keyLock(key string) *sync.Mutex {
	hash := sha256.Sum256([]byte(l.dir + "\x00" + key))
	return &localKeyLocks[int(hash[0])%len(localKeyLocks)]
}
