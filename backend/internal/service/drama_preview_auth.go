package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"video-canvas/internal/model"
)

const (
	dramaPreviewTTL          = 10 * time.Minute
	dramaPreviewFailCooldown = time.Minute
)

type dramaPreviewKey struct {
	episodeID uint64
	key       string
}

// dramaPreviewIdentity 固定建票时的渲染器、集版本与输出请求。
type dramaPreviewIdentity struct {
	User, Drama, EpisodeID uint64
	Episode                int
	AspectRatio            string
	RenderVersion          int
	Req                    model.DramaPreviewReq
}

type dramaPreviewTicket struct {
	user           uint64
	drama          uint64
	episode        int
	episodeID      uint64
	segmentVersion int64
	manifestHash   string
	aspectRatio    string
	renderVersion  int
	stageC         *dramaManifestV2Source
	v2             *dramaV2Resolution
	key            string
	id             string
	path           string
	status         string
	err            error
	expires        time.Time
	ctx            context.Context
	cancel         context.CancelFunc
}

const maxConcurrentPreviewRenders = 4
const maxManifestErrEntries = 256

type manifestWarmupErr struct {
	err error
	at  time.Time
	seq uint64
}

type dramaPreviewStore struct {
	mu               sync.Mutex
	secret           []byte
	tickets          map[string]*dramaPreviewTicket
	byKey            map[dramaPreviewKey]*dramaPreviewTicket
	manifestInFlight map[string]struct{}
	manifestErr      map[string]manifestWarmupErr
	manifestErrSeq   uint64
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	sem              chan struct{}
	closed           bool
	now              func() time.Time
	execTimeout      time.Duration
	readyTTL         time.Duration
	failTTL          time.Duration
}

func (p *dramaPreviewStore) manifestErrTTL() time.Duration {
	if p.failTTL > 0 {
		return p.failTTL
	}
	return dramaPreviewFailCooldown
}

// newDramaPreviewStore 使用配置密钥的独立派生域；缺省生成进程级随机密钥，不使用固定回退值。
func newDramaPreviewStore(secret string) *dramaPreviewStore {
	key := make([]byte, 32)
	if secret == "" {
		if _, err := rand.Read(key); err != nil {
			panic("无法生成预览签名密钥")
		}
	} else {
		sum := sha256.Sum256([]byte("vc-preview-v1:" + secret))
		copy(key, sum[:])
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &dramaPreviewStore{
		secret:           key,
		tickets:          map[string]*dramaPreviewTicket{},
		byKey:            map[dramaPreviewKey]*dramaPreviewTicket{},
		manifestInFlight: map[string]struct{}{},
		manifestErr:      map[string]manifestWarmupErr{},
		ctx:              ctx,
		cancel:           cancel,
		sem:              make(chan struct{}, maxConcurrentPreviewRenders),
		now:              time.Now,
		execTimeout:      dramaComposeTimeout,
		readyTTL:         dramaPreviewTTL,
		failTTL:          dramaPreviewFailCooldown,
	}
}

type dramaPreviewClaims struct {
	User    uint64 `json:"u"`
	Drama   uint64 `json:"d,omitempty"`
	Episode int    `json:"e,omitempty"`
	Ticket  string `json:"t,omitempty"`
	Expires int64  `json:"x"`
	Purpose string `json:"p"`
}

// sign 将用途与身份全部包含在 HMAC 内，防止 cookie 与 stream token 相互替代。
func (p *dramaPreviewStore) sign(c dramaPreviewClaims) string {
	raw, _ := json.Marshal(c)
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, p.secret)
	_, _ = mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify 拒绝错误签名、未知用途和已过期凭证。
func (p *dramaPreviewStore) verify(raw, purpose string) (dramaPreviewClaims, error) {
	var c dramaPreviewClaims
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return c, errors.New("无效预览凭证")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, err
	}
	mac := hmac.New(sha256.New, p.secret)
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return c, errors.New("无效预览签名")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(body, &c); err != nil {
		return c, err
	}
	if c.Purpose != purpose || c.User == 0 || c.Expires <= p.now().Unix() {
		return c, errors.New("预览凭证已失效")
	}
	return c, nil
}
