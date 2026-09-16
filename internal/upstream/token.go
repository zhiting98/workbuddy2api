// token.go 幂等令牌（前端 randomUUID 同款语义），供上报事件 requestId/draw_uuid 等复用。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// clientToken 幂等令牌（前端 randomUUID 同款语义）。
func clientToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}
