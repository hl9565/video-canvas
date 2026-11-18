package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// OptionalAssetID 区分 PATCH 中省略、显式清空和指定素材三种状态。
type OptionalAssetID struct {
	Set   bool
	Value *uint64
}

// UnmarshalJSON 接受 null 或 bigint 正整数素材 ID；空串不是清空指令。
func (a *OptionalAssetID) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*a = OptionalAssetID{Set: true}
		return nil
	}
	var id AssetID
	if err := json.Unmarshal(data, &id); err != nil {
		return &DramaVisualError{Cause: err}
	}
	value := uint64(id)
	if !validDramaAssetID(value) {
		return &DramaVisualError{Msg: "still_asset_id 必须为 null 或 1 到 9223372036854775807 之间的十进制素材 ID"}
	}
	*a = OptionalAssetID{Set: true, Value: &value}
	return nil
}

// MarshalJSON 将显式清空编码为 null，有值时输出十进制数字。
func (a OptionalAssetID) MarshalJSON() ([]byte, error) { return json.Marshal(a.Value) }

// UnmarshalJSON 严格解码特效对象，拒绝未支持的字段与越界值。
func (fx *DramaSegmentFX) UnmarshalJSON(data []byte) error {
	type plain DramaSegmentFX
	var decoded *plain
	if err := DecodeDramaJSON(bytes.NewReader(data), &decoded); err != nil {
		return &DramaVisualError{Cause: err}
	}
	if decoded == nil {
		return &DramaVisualError{Msg: "fx 必须是 JSON 对象"}
	}
	value := DramaSegmentFX(*decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*fx = value
	return nil
}

// DecodeDramaJSON 只读取一个 JSON 值，确保调用方在读取请求流时也拒绝尾随内容。
func DecodeDramaJSON(r io.Reader, dst any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	err := decoder.Decode(new(json.RawMessage))
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("请求体只能包含一个 JSON 值")
	}
	return err
}
