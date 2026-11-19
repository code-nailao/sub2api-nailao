package service

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Keep the external SSE connection alive during tool execution. Incremental
// content and this heartbeat serialize all public writes through mu.
// Stop joins the writer before any terminal events or errors are emitted.
type deepSeekHeartbeat struct {
	mu   sync.Mutex
	once sync.Once
	stop chan struct{}
	done chan struct{}
}

func startDeepSeekHeartbeat(c *gin.Context, stream bool) *deepSeekHeartbeat {
	h := &deepSeekHeartbeat{}
	if !stream {
		return h
	}
	h.stop, h.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-c.Request.Context().Done():
				return
			case <-ticker.C:
				h.mu.Lock()
				c.Writer.Header().Set("Content-Type", "text/event-stream")
				c.Writer.Header().Set("Cache-Control", "no-cache")
				c.Writer.Header().Set("X-Accel-Buffering", "no")
				if _, err := fmt.Fprint(c.Writer, ": keepalive\n\n"); err != nil {
					h.mu.Unlock()
					return
				}
				c.Writer.Flush()
				h.mu.Unlock()
			}
		}
	}()
	return h
}
func (h *deepSeekHeartbeat) Stop() {
	if h == nil || h.stop == nil {
		return
	}
	h.once.Do(func() { close(h.stop); <-h.done })
}
func writeDeepSeekStreamError(c *gin.Context, protocol string, result *OpenAIForwardResult) {
	message := "DeepSeek is temporarily unavailable. Please try again later."
	if result != nil && len(result.DeepSeekModelTurns) > 0 {
		message = "DeepSeek could not finish this response. Any partial output and confirmed usage are retained; the request was not replayed."
	}
	MarkOpsStreamError(c, "upstream_error", message, 502)
	payload := map[string]any{"type": "error", "error": map[string]string{"type": "upstream_error", "message": message}}
	wire, _ := json.Marshal(payload)
	if protocol == "chat" {
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", wire)
	} else {
		_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", wire)
	}
	c.Writer.Flush()
}
