package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"time"

	"go.uber.org/zap"

	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/storage"
)

const (
	localVariantConcurrent = 2
	localVariantMaxPending = 64
	localVariantTimeout    = 15 * time.Second
	localVariantCooldown   = 30 * time.Second
	localVariantMaxFailed  = 2048
)

// LocalPreviewEncoder 负责选择编码格式并将一帧预览流式写出。
type LocalPreviewEncoder interface {
	// Format 决定磁盘变体扩展名，必须与后续 Encode 的实际编码一致。
	Format(ctx context.Context) (videoframe.PreviewInfo, error)
	// Encode 从本地源编码一帧；ctx 取消时必须停止编码并退出。
	Encode(ctx context.Context, srcPath string, dst io.Writer) (videoframe.PreviewInfo, error)
}

// LocalVariantService 管理本地素材的缓存命中和有界预览生成任务。
type LocalVariantService struct {
	root    context.Context
	cancel  context.CancelFunc
	encoder LocalPreviewEncoder
	slots   chan struct{}
	jobs    sync.WaitGroup

	mu      sync.Mutex
	pending int
	tasks   map[string]*localVariantTask
	failed  map[string]localVariantFailure
}

type localVariantTask struct {
	ctx     context.Context
	cancel  context.CancelFunc
	store   *storage.LocalStorage
	source  storage.LocalSourceInfo
	key     string
	variant string
	id      string
	wait    bool

	done chan struct{}
	file *storage.LocalFile
	err  error
}

type localVariantFailure struct {
	err     error
	expires time.Time
}

// NewLocalVariantService 创建独立的预览调度器，应用关闭时须调用 Close。
func NewLocalVariantService(rootCtx context.Context, encoder LocalPreviewEncoder) *LocalVariantService {
	// 1. 共享任务归应用所有，不继承首个浏览器请求的生命周期。
	root, cancel := context.WithCancel(rootCtx)
	return &LocalVariantService{
		root: root, cancel: cancel, encoder: encoder, slots: make(chan struct{}, localVariantConcurrent),
		tasks: make(map[string]*localVariantTask), failed: make(map[string]localVariantFailure),
	}
}

// Close 停止接单、取消已接收任务并等待生成及临时文件清理结束，可重复调用。
func (s *LocalVariantService) Close() {
	// 1. 与接单共用互斥锁，保证 Wait 开始后不会再增加任务。
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()
	// 2. 不持锁等待，以便运行中的任务完成回收。
	s.jobs.Wait()
}

// Resolve 命中磁盘或合并同一指纹的预览请求，浏览器取消只停止自己的等待。
func (s *LocalVariantService) Resolve(ctx context.Context, local *storage.LocalStorage, key, variant string) (*storage.LocalFile, error) {
	// 1. 不为已取消的请求启动新任务，磁盘命中也不占编码名额。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.root.Err() != nil {
		return nil, errcode.ErrVariantUnavailable
	}
	source, err := local.StatSource(key)
	if err != nil {
		return nil, err
	}
	file, err := findLocalVariant(local, key, variant)
	if file != nil || err != nil {
		return file, err
	}

	// 2. 首次接单才计入等待队列；所有同指纹请求共用同一个完成信号，不按请求额外分配 channel。
	id := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", local.RootDir(), key, source.Size, source.MTimeNano, variant)
	task, err := s.submit(localVariantTask{store: local, key: key, variant: variant, source: source, id: id})
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-task.done:
		return task.file, task.err
	}
}

// findLocalVariant 复用先前发布的任一支持格式，不依赖当前是否还安装编码器。
func findLocalVariant(local *storage.LocalStorage, key, variant string) (*storage.LocalFile, error) {
	for _, ext := range [...]string{"webp", "jpeg"} {
		file, hit, err := local.LookupVariant(key, variant, ext)
		if hit || err != nil {
			return file, err
		}
	}
	return nil, nil
}

// submit 在启动 goroutine 前预留运行或排队名额，重复请求直接复用已有任务及其 done 信号。
func (s *LocalVariantService) submit(input localVariantTask) (*localVariantTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root.Err() != nil {
		return nil, errcode.ErrVariantUnavailable
	}
	s.pruneFailures(time.Now())
	if failure, ok := s.failed[input.id]; ok {
		return nil, failure.err
	}
	task := s.tasks[input.id]
	if task == nil {
		select {
		case s.slots <- struct{}{}:
		default:
			if s.pending >= localVariantMaxPending {
				return nil, errcode.ErrVariantUnavailable
			}
			s.pending++
			input.wait = true
		}
		input.ctx, input.cancel = context.WithTimeout(s.root, localVariantTimeout)
		input.done = make(chan struct{})
		task = &input
		s.tasks[input.id] = task
		s.jobs.Add(1)
		go s.run(task)
	}
	return task, nil
}

// run 等候配额并生成预览；任务总时限包含排队、探测、编码和发布。
func (s *LocalVariantService) run(task *localVariantTask) {
	var (
		file *storage.LocalFile
		err  error
	)
	defer s.jobs.Done()
	defer task.cancel()
	defer func() {
		task.file, task.err = file, err
		s.finish(task, err)
		close(task.done)
	}()
	if task.wait && !s.waitForSlot(task.ctx) {
		err = errcode.ErrVariantUnavailable
		return
	}
	defer func() { <-s.slots }()
	if task.ctx.Err() != nil {
		err = errcode.ErrVariantUnavailable
		return
	}
	file, err = s.generate(task)
	if task.ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, storage.ErrSourceChanged) || errors.Is(err, videoframe.ErrBusy) || errors.Is(err, videoframe.ErrTimeout) {
		file, err = nil, errcode.ErrVariantUnavailable
	}
}

// waitForSlot 退出等待时归还队列名额，即便任务在队列中先到期。
func (s *LocalVariantService) waitForSlot(ctx context.Context) bool {
	acquired := false
	select {
	case s.slots <- struct{}{}:
		acquired = true
	case <-ctx.Done():
	}
	s.mu.Lock()
	s.pending--
	s.mu.Unlock()
	return acquired
}

// generate 进场后再次查缓存，避免在首次查找与接单之间已有任务发布而重复编码。
func (s *LocalVariantService) generate(task *localVariantTask) (*storage.LocalFile, error) {
	file, err := findLocalVariant(task.store, task.key, task.variant)
	if file != nil || err != nil {
		return file, err
	}
	format, err := s.encoder.Format(task.ctx)
	if err != nil {
		return nil, err
	}
	return task.store.PublishVariantContext(task.ctx, task.key, task.variant, format.Format, task.source, func(dst io.Writer) error {
		if err := task.ctx.Err(); err != nil {
			return err
		}
		actual, err := s.encoder.Encode(task.ctx, task.source.Path, dst)
		if err != nil {
			return err
		}
		if actual != format {
			return videoframe.ErrDecode
		}
		return nil
	})
}

// finish 在开放同指纹的新接单前先记失败并移除任务登记；既有等待者通过关闭 task.done 读取结果。
func (s *LocalVariantService) finish(task *localVariantTask, err error) {
	s.mu.Lock()
	delete(s.tasks, task.id)
	if task.ctx.Err() == nil && deterministicLocalVariantFailure(err) {
		s.rememberFailure(task.id, err, time.Now())
	}
	s.mu.Unlock()
	if err != nil && !errors.Is(err, errcode.ErrVariantUnavailable) {
		logger.Debug("本地素材变体生成失败", zap.String("variant", task.variant), zap.Error(err))
	}
}

// deterministicLocalVariantFailure 排除取消、队列压力及临时 I/O 错误。
func deterministicLocalVariantFailure(err error) bool {
	return errors.Is(err, storage.ErrNotFound) || errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, videoframe.ErrUnavailable) || errors.Is(err, videoframe.ErrDecode) || errors.Is(err, videoframe.ErrTooLarge)
}

// pruneFailures 淘汰冷却到期项，调用方需持有调度锁。
func (s *LocalVariantService) pruneFailures(now time.Time) {
	for id, failure := range s.failed {
		if !failure.expires.After(now) {
			delete(s.failed, id)
		}
	}
}

// rememberFailure 在有界冷却表中记录失败，满时淘汰最早写入的项。
func (s *LocalVariantService) rememberFailure(id string, err error, now time.Time) {
	s.pruneFailures(now)
	if len(s.failed) >= localVariantMaxFailed {
		var oldestID string
		var oldest time.Time
		for key, failure := range s.failed {
			if oldest.IsZero() || failure.expires.Before(oldest) {
				oldestID, oldest = key, failure.expires
			}
		}
		delete(s.failed, oldestID)
	}
	s.failed[id] = localVariantFailure{err: err, expires: now.Add(localVariantCooldown)}
}
