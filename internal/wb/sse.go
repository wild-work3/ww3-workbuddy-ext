package wb

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// errIdleTimeout 表示上游长时间无数据（空闲看门狗触发）。
var errIdleTimeout = errors.New("idle timeout")

// maxSSELine 是单条 SSE 行的上限（大 tool_call 参数可能很长）。
const maxSSELine = 8 << 20

// sseReader 逐帧读上游 SSE，带空闲看门狗。
//
// 为什么不用 http.Client.Timeout：整体超时会掐断长回答（旧版 issue #42）。
// 这里只在「超过 idle 没有任何数据」时才判定失败。
type sseReader struct {
	lines  chan string
	errs   chan error
	idle   time.Duration
	cancel context.CancelFunc
}

// newSSEReader 起后台读取协程（line 级，冒号后空格可选见 sseDataPayload）。
func newSSEReader(r io.Reader, idle time.Duration) *sseReader {
	ctx, cancel := context.WithCancel(context.Background())
	s := &sseReader{lines: make(chan string, 64), errs: make(chan error, 1), idle: idle, cancel: cancel}
	go func() {
		defer close(s.lines)
		br := bufio.NewReaderSize(r, 64<<10)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				select {
				case s.lines <- strings.TrimRight(line, "\r\n"):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					select {
					case s.errs <- err:
					default:
					}
				}
				return
			}
		}
	}()
	return s
}

// Next 返回下一条 data 帧内容；io.EOF 表示流结束。
func (s *sseReader) Next() (string, error) {
	timer := time.NewTimer(s.idle)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				// 读取协程结束：可能有残留错误
				select {
				case err := <-s.errs:
					return "", err
				default:
					return "", io.EOF
				}
			}
			if payload, isData := sseDataPayload(line); isData {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(s.idle)
				if payload == "" {
					continue
				}
				return payload, nil
			}
		case err := <-s.errs:
			return "", err
		case <-timer.C:
			s.cancel()
			return "", errIdleTimeout
		}
	}
}

// Close 结束读取协程。
func (s *sseReader) Close() { s.cancel() }

// sseDataPayload 是入站帧解析的唯一入口：只认 data: 前缀，**冒号后空格可选**。
//
// 历史事故：只认 "data: "（带空格）时，无空格帧不计有效帧 → 空流兜底 → 健康账号被冷却。
// 因此禁止各处自写 HasPrefix。
func sseDataPayload(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(line[len("data:"):]), true
}
