// Package jsonhash 提供确定性规范化 JSON 序列化与 SHA-256 指纹计算。
package jsonhash

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// CanonicalJSON 将任意 Go 值先按标准 json 标签展开，再按字典序递归排序全部对象键，输出紧凑无空白字节流。
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CanonicalSHA256 返回 CanonicalJSON(v) 的小写十六进制 SHA-256 摘要。
func CanonicalSHA256(v any) (string, error) {
	b, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return SHA256Hex(b), nil
}

// SHA256Hex 返回字节切片的小写十六进制 SHA-256 摘要。
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case map[string]any:
		return writeCanonicalMap(buf, x)
	case []any:
		return writeCanonicalSlice(buf, x)
	case json.Number:
		buf.WriteString(normalizeNumber(x.String()))
		return nil
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	}
}

func writeCanonicalMap(buf *bytes.Buffer, m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		if err := writeCanonical(buf, m[k]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func writeCanonicalSlice(buf *bytes.Buffer, xs []any) error {
	buf.WriteByte('[')
	for i, item := range xs {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeCanonical(buf, item); err != nil {
			return err
		}
	}
	buf.WriteByte(']')
	return nil
}

func normalizeNumber(s string) string {
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
