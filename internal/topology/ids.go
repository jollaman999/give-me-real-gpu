package topology

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
)

// stableSum 은 이름 조각들을 NUL 로 이어 sha1 을 낸다.
// NUL 로 잇기 때문에 ("ab", "c") 와 ("a", "bc") 가 같은 값이 되지 않는다.
func stableSum(parts []string) [sha1.Size]byte {
	return sha1.Sum([]byte(strings.Join(parts, "\x00")))
}

// StableUUID 는 이름 조각에서 결정적으로 UUID 를 만든다.
// sha1 앞 16바이트에 버전 니블 5, variant 10 을 넣은 8-4-4-4-12 소문자 형식이다.
// 같은 이름이면 재기동해도 같은 id 가 나오므로 관리 플랫폼과 모니터링 서버에 쌓인 행이 유지된다.
func StableUUID(parts ...string) string {
	sum := stableSum(parts)
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// StableHex32 는 이름 조각에서 결정적으로 32자 hex 를 만든다 (keystone project·user id 형식).
func StableHex32(parts ...string) string {
	sum := stableSum(parts)
	return hex.EncodeToString(sum[:16])
}
